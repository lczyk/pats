package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// InitOptions configures `pats init`.
type InitOptions struct {
	ConfigDir string
	Out       io.Writer
	Now       time.Time
	Suites    []string // run's selectors: the sandboxes these pairs use
	Agents    []string
	Tasks     []string
	Sandboxes []string // narrow to these sandbox ids
	Driver    string   // narrow to sandboxes of this driver
	Force     bool     // rebuild and re-pull what exists
}

// Init makes every image the selected pairs' sandboxes need and pins it in
// .pats/images.json, which `pats run` starts from. a sandbox that fails does
// not stop the others; their pins are still written.
func Init(cfg *config.Config, opts InitOptions) error {
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	ro := Options{ConfigDir: opts.ConfigDir, Out: opts.Out, Color: useColor(opts.Out)}
	lg := logw{opts.Out, ro.Color}

	sbs, err := initSandboxes(cfg, opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer sandbox.ShutdownLXD()

	type pinned struct {
		target string
		sb     config.Sandbox
		e      *pinEntry
	}
	var done []pinned
	var failed []string
	for _, sb := range sbs {
		lg.info("sandbox %q: making its images (%s)", sb.ID, sb.Driver)
		target, e, err := initSandbox(ctx, sb, ro, opts)
		if err != nil {
			lg.error("sandbox %q: %v", sb.ID, err)
			failed = append(failed, sb.ID)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		lg.info("sandbox %q: pinned %s under %s", sb.ID, short(e.Image.ID), target)
		done = append(done, pinned{target, sb, e})
	}

	keep := map[string]bool{}
	for _, sb := range cfg.Sandboxes {
		keep[sb.ID] = true
	}
	// another project's init may remove an image this one found or made
	// before it is pinned; under the lock, what is still there is pinned
	// and what is gone is a failure to rerun.
	unlock, lerr := machineLock()
	if lerr != nil {
		lg.warn("cannot take %s.lock: %v; removing nothing", projectsFile(), lerr)
		unlock = func() {}
	}
	defer unlock()
	var projects []string
	if lerr == nil {
		if projects, lerr = registerProject(opts.ConfigDir); lerr != nil {
			lg.warn("cannot register this project in %s: %v; removing nothing", projectsFile(), lerr)
		}
	}
	err = updatePins(opts.ConfigDir, func(f *pinFile) error {
		for _, p := range done {
			if gone := missingImage(ctx, hostFor(p.sb), p.e); gone != "" {
				lg.error("sandbox %q: %s was removed since this init made it, by another project's init; run pats init again", p.sb.ID, short(gone))
				failed = append(failed, p.sb.ID)
				continue
			}
			f.setPin(p.target, p.sb.ID, p.e, opts.Now)
		}
		f.dropMissing(keep, opts.Now)
		return nil
	})
	if err != nil {
		return err
	}
	// removal only once the pins that stop needing those images are on
	// disk: a failed write leaves the old pins and their images both there.
	// a failed write after a removal leaves listed an image that is gone,
	// which the next init drops.
	if lerr == nil {
		err = updatePins(opts.ConfigDir, func(f *pinFile) error {
			removeSuperseded(ctx, f, opts.ConfigDir, projects, opts.Now, lg)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("init failed for sandbox(es) %s", strings.Join(failed, ", "))
	}
	return nil
}

// initSandboxes is what init makes: the sandboxes the selected pairs use,
// narrowed by --sandbox and --driver.
func initSandboxes(cfg *config.Config, opts InitOptions) ([]config.Sandbox, error) {
	pairs, err := cfg.ExpandTestPairs(opts.Suites...)
	if err != nil {
		return nil, err
	}
	if len(opts.Agents) > 0 || len(opts.Tasks) > 0 {
		if pairs, err = cfg.FilterPairs(pairs, opts.Agents, opts.Tasks); err != nil {
			return nil, err
		}
	}
	used := usedSandboxes(cfg, pairs)
	want := map[string]bool{}
	for _, id := range opts.Sandboxes {
		if !used[id] {
			return nil, fmt.Errorf("--sandbox %q: no selected pair uses it", id)
		}
		want[id] = true
	}
	var out []config.Sandbox
	for _, sb := range cfg.Sandboxes {
		if !used[sb.ID] || (len(want) > 0 && !want[sb.ID]) || (opts.Driver != "" && sb.Driver != opts.Driver) {
			continue
		}
		out = append(out, sb)
	}
	if len(out) == 0 {
		return nil, errors.New("no sandbox to init: the selection uses none")
	}
	return out, nil
}

// initSandbox makes one sandbox's images and returns its pin.
func initSandbox(ctx context.Context, sb config.Sandbox, ro Options, opts InitOptions) (string, *pinEntry, error) {
	h := hostFor(sb)
	target, daemon, err := h.target(ctx)
	if err != nil {
		return "", nil, err
	}
	inputs, err := localInputs(sb, ro.ConfigDir)
	if err != nil {
		return "", nil, err
	}
	e := &pinEntry{
		Daemon:      daemon,
		PatsVersion: version.Info.Version,
		PatsBuild:   patsBuild(),
		Built:       opts.Now.UTC().Format(time.RFC3339),
		Inputs:      inputs,
	}
	switch p, ok := h.(puller); {
	case sb.Driver == "lxd":
		err = initLXD(ctx, sb, ro, opts, lxdOptions(sb), e)
	case ok:
		err = initContainer(ctx, sb, ro, opts, p, e)
	default:
		err = fmt.Errorf("driver %q has no images to init", sb.Driver)
	}
	if err == nil {
		e.Components, err = h.stamps(ctx, e.Image.ID)
	}
	if err == nil {
		upstreamInputs(ctx, sb, ro.ConfigDir, h, e)
	}
	return target, e, err
}

// initContainer makes a docker or podman sandbox's image and proxy: built
// from build: or base / provision, else pulled when absent (always, with
// --force).
func initContainer(ctx context.Context, sb config.Sandbox, ro Options, opts InitOptions, h puller, e *pinEntry) error {
	lg := logw{ro.Out, ro.Color}
	logPath := initLogPath(ro.ConfigDir, sb.ID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logF, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logF.Close()
	// a failure's error carries the log's tail; the whole log stays.
	fail := func(err error) error {
		return fmt.Errorf("%w\n(full log: %s)", err, relToCwd(logPath))
	}
	warned := false
	base := func() string {
		img, warn := sandbox.SandboxImage("")
		if warn != "" && !warned {
			lg.warn("%s", warn)
			warned = true
		}
		return img
	}
	// the build resolves its FROM tags itself, somewhere between these two
	// reads: a base the same on both is the one it used.
	before := baseDigests(ctx, sb, ro.ConfigDir, h, logF)
	defer func() {
		held, moved := stableBases(before, baseDigests(ctx, sb, ro.ConfigDir, h, logF))
		for ref, digest := range held {
			e.Inputs[upstreamPrefix+ref] = digest
		}
		for _, ref := range moved {
			lg.warn("sandbox %q: its base %s moved while init built on it; its upstream is not tracked until the next pats init", sb.ID, ref)
		}
	}()
	switch {
	case provisions(sb):
		id, _, err := provisionedContainerImage(ctx, sb, ro, base, opts.Force, logF)
		if err != nil {
			return fail(err)
		}
		e.Image = pinImage{ID: id, Built: true}
	case sb.Build != "":
		ctxDir, dockerfile, err := resolveBuildSpec(ro.ConfigDir, sb.Build)
		if err != nil {
			return err
		}
		id, _, err := sandbox.BuildContainerImage(ctx, sb.Driver, sandbox.ContainerBuild{Dir: ctxDir, Dockerfile: dockerfile, Force: opts.Force, Log: logF}, base)
		if err != nil {
			return fail(err)
		}
		e.Image = pinImage{ID: id, Built: true}
	case sb.Image != "":
		im, err := ensurePulled(ctx, h, sb.Image, opts.Force, lg, logF)
		if err != nil {
			return err
		}
		e.Image = im
	default:
		im, err := ensurePulled(ctx, h, base(), opts.Force, lg, logF)
		if err != nil {
			return err
		}
		e.Image = im
	}
	if sb.Egress.Set {
		ref, warn := sandbox.ProxyImage(sb.Egress.Image)
		if warn != "" {
			lg.warn("%s", warn)
		}
		im, err := ensurePulled(ctx, h, ref, opts.Force, lg, logF)
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		e.Proxy = &im
	}
	return nil
}

// ensurePulled pins ref, pulling it when absent or when forced. the image is
// pats's own only when this pull brought it: one already there, even a
// published pats image, may be the user's (a local `make images` build).
func ensurePulled(ctx context.Context, h puller, ref string, force bool, lg logw, log io.Writer) (pinImage, error) {
	// what the daemon held before: an image a pull lands on that was
	// already there, tagged or not, is not pats's.
	had, err := h.ids(ctx)
	if err != nil {
		return pinImage{}, err
	}
	pulled := false
	if force || !h.present(ctx, ref) {
		lg.info("pulling %s", ref)
		if err := h.pull(ctx, ref, log); err != nil {
			return pinImage{}, err
		}
		pulled = true
	}
	id, digest, err := h.inspect(ctx, ref)
	if err != nil {
		return pinImage{}, err
	}
	return pinImage{ID: id, Name: ref, Digest: digest, Pulled: pulled && !had[id]}, nil
}

// initLXD makes an lxd sandbox's layers and proxy on its server, logging to
// .pats/init/<sandbox>.log, and pins them by fingerprint.
func initLXD(ctx context.Context, sb config.Sandbox, ro Options, opts InitOptions, o sandbox.Options, e *pinEntry) error {
	switch {
	case strings.Contains(sb.Image, ":"):
		// an image of another remote: copied onto the server now, so a run
		// never downloads it.
		fp, copied, err := sandbox.LXDCopyImage(ctx, o, sb.Image, opts.Force)
		if err != nil {
			return err
		}
		e.Image = pinImage{ID: fp, Name: sb.Image, Pulled: copied}
	case sb.Image != "":
		fp, err := sandbox.LXDFingerprint(ctx, o, sb.Image)
		if err != nil {
			return err
		}
		e.Image = pinImage{ID: fp, Name: sb.Image}
	default:
		logPath := initLogPath(ro.ConfigDir, sb.ID)
		alias, layers, _, upstream, err := lxdImage(ctx, sb, ro, logPath, opts.Force)
		if err != nil {
			return err
		}
		for ref, fp := range upstream {
			e.Inputs[upstreamPrefix+ref] = fp
		}
		fp, err := sandbox.LXDFingerprint(ctx, o, alias)
		if err != nil {
			return err
		}
		e.Image = pinImage{ID: fp, Name: alias, Built: true}
		for _, l := range layers {
			lfp, err := sandbox.LXDFingerprint(ctx, o, l)
			if err != nil {
				return err
			}
			e.Layers = append(e.Layers, lfp)
		}
	}
	if sb.Egress.Set {
		po := o
		lg := logw{ro.Out, ro.Color}
		po.Progress = func(msg string) { lg.info("sandbox %q: %s", sb.ID, msg) }
		po.Warn = func(msg string) { lg.warn("sandbox %q: %s", sb.ID, msg) }
		po.ForceImages = opts.Force
		alias, err := sandbox.LXDProxyImage(ctx, po)
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		fp, err := sandbox.LXDFingerprint(ctx, o, alias)
		if err != nil {
			return err
		}
		e.Proxy = &pinImage{ID: fp, Name: alias, Built: true}
	}
	return nil
}

// usePins points each sandbox the pairs use at its pinned images, and stops
// the run, naming the init that fixes it, when one is not pinned for where
// the run would start it, or its image is gone.
func usePins(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair) ([]upstreamCheck, error) {
	pins, err := readPins(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	var checks []upstreamCheck
	used := usedSandboxes(cfg, pairs)
	lg := logw{opts.Out, opts.Color}
	var missing []string
	var reasons []string
	for i, sb := range cfg.Sandboxes {
		if !used[sb.ID] {
			continue
		}
		h := hostFor(sb)
		target, daemon, err := h.target(ctx)
		if err != nil {
			return nil, fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		e := pins.entry(target, sb.ID)
		reason := ""
		switch {
		case e == nil:
			reason = "not pinned for " + target
		case e.Daemon != daemon:
			reason = "pinned against another " + sb.Driver + " daemon"
		case !h.present(ctx, e.Image.ID):
			reason = "its pinned image " + short(e.Image.ID) + " is gone"
		case sb.Egress.Set && e.Proxy == nil:
			reason = "no egress proxy pinned"
		case sb.Egress.Set && !h.present(ctx, e.Proxy.ID):
			reason = "its pinned egress proxy " + short(e.Proxy.ID) + " is gone"
		case sb.Egress.Set:
			reason = proxyMismatch(e)
		}
		if reason != "" {
			missing = append(missing, sb.ID)
			reasons = append(reasons, fmt.Sprintf("  %s: %s", sb.ID, reason))
			continue
		}
		if now, err := localInputs(sb, opts.ConfigDir); err != nil {
			lg.warn("sandbox %q: cannot tell whether its image is current: %v", sb.ID, err)
		} else if changed := staleLocal(e, now); len(changed) > 0 {
			lg.warn("sandbox %q: changed since its pats init: %s; running the pinned image (pats init --sandbox %s to rebuild)",
				sb.ID, strings.Join(changed, ", "), sb.ID)
		}
		if drift := staleComponents(e.Components); len(drift) > 0 {
			lg.warn("sandbox %q: its harnesses differ from what this pats pins: %s (pats init --sandbox %s to rebuild)",
				sb.ID, strings.Join(drift, ", "), sb.ID)
		}
		cfg.Sandboxes[i].Image = e.Image.ID
		cfg.Sandboxes[i].Components = e.Components
		checks = append(checks, upstreamCheck{sb: sb.ID, lxd: sb.Driver == "lxd", inputs: e.Inputs})
		if e.Proxy != nil {
			cfg.Sandboxes[i].ProxyImage = e.Proxy.ID
		}
	}
	if len(missing) == 0 {
		return checks, nil
	}
	cmd := "pats init"
	if len(missing) < len(used) {
		sort.Strings(missing)
		for _, id := range missing {
			cmd += " --sandbox " + id
		}
	}
	return nil, fmt.Errorf("images missing for %d sandbox(es):\n%s\nrun `%s` first", len(missing), strings.Join(reasons, "\n"), cmd)
}

// missingImage is an image e pins that its host no longer has, "" when
// it has them all.
func missingImage(ctx context.Context, h imageHost, e *pinEntry) string {
	for _, im := range e.images() {
		if !h.present(ctx, im.ID) {
			return im.ID
		}
	}
	return ""
}

// initLogPath is where init logs a sandbox's builds and pulls.
func initLogPath(configDir, sb string) string {
	return filepath.Join(configDir, ".pats", "init", sb+".log")
}

// short is an image id or fingerprint cut to its first 12 hex digits.
func short(id string) string {
	h := strings.TrimPrefix(id, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
