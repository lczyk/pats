package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/machine"
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
	Sandboxes []string // narrow to these sandbox names
	Driver    string   // narrow to sandboxes of this driver
	Force     bool     // rebuild and re-pull what exists
	Verbose   int      // -v count: 1 adds build steps, 2 streams the builds
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
	ro := Options{ConfigDir: opts.ConfigDir, Out: opts.Out, Color: useColor(opts.Out), Verbose: opts.Verbose}
	lg := logw{opts.Out, ro.Color, ro.Verbose}
	if n := cfg.Harnesses.Note(); n != "" {
		lg.info("%s", n)
	}

	sbs, err := initSandboxes(cfg, opts)
	if err != nil {
		return err
	}
	// a run reads its pins once and starts each pair from them: init in the
	// same project waits for no run, so it may not remove an image one uses.
	unlockDir, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return err
	}
	defer unlockDir()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer sandbox.Shutdown()

	type pinned struct {
		target string
		sb     config.Sandbox
		e      *pinEntry
	}
	var done []pinned
	var failed []string
	for _, sb := range sbs {
		if sandbox.TraitsOf(sb.Driver).NoImages {
			lg.info("sandbox %q: host driver, nothing to make: its agents run this machine's own clis", sb.Name)
			continue
		}
		clis := sandboxCLIs(cfg, sb.Name)
		lg.info("sandbox %q: %s", sb.Name, initWhat(sb, opts.ConfigDir, places(sb) && len(clis) > 0))
		target, e, err := initSandbox(ctx, sb, clis, sandboxPins(cfg, sb.Name), ro, opts)
		if err != nil {
			failed = append(failed, sb.Name)
			if ctx.Err() != nil {
				lg.error("sandbox %q: interrupted", sb.Name)
				break
			}
			lg.error("sandbox %q: %v", sb.Name, err)
			continue
		}
		lg.info("sandbox %q: pinned %s under %s", sb.Name, short(e.Image.ID), target)
		done = append(done, pinned{target, sb, e})
	}

	keep := map[string]bool{}
	for _, sb := range cfg.Sandboxes {
		keep[sb.Name] = true
	}
	// another project's init may remove an image this one found or made
	// before it is pinned; under the lock, what is still there is pinned
	// and what is gone is a failure to rerun.
	// without the lock the check below races another init's removal, and an
	// unregistered project's pins are invisible to the next one's.
	unlock, err := machineLock(ctx)
	if err != nil {
		return fmt.Errorf("cannot take %s.lock: %w; nothing pinned", projectsFile(), err)
	}
	defer unlock()
	projects, err := registerProject(opts.ConfigDir)
	if err != nil {
		return fmt.Errorf("cannot register this project in %s: %w; nothing pinned", projectsFile(), err)
	}
	err = updatePins(opts.ConfigDir, func(f *pinFile) error {
		for _, p := range done {
			h, err := hostFor(p.sb)
			gone := ""
			if err == nil {
				gone = missingImage(ctx, h, p.e)
			}
			if gone != "" {
				lg.error("sandbox %q: %s was removed since this init made it, by another project's init; run pats init again", p.sb.Name, short(gone))
				failed = append(failed, p.sb.Name)
				continue
			}
			if tree, ok := missingTree(h, p.e); err == nil && ok {
				lg.error("sandbox %q: harness tree %s was removed since this init placed it, by a pats clean; run pats init again", p.sb.Name, short(tree))
				failed = append(failed, p.sb.Name)
				continue
			}
			f.setPin(p.target, p.sb.Name, p.e, opts.Now)
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
	err = updatePins(opts.ConfigDir, func(f *pinFile) error {
		removeSuperseded(ctx, f, opts.ConfigDir, projects, opts.Now, lg)
		return nil
	})
	if err != nil {
		return err
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
	// the judges of the selected suites' agent scorers run in sandboxes too;
	// -a and -t narrow the task pairs, not them.
	judges, err := judgePairsOf(cfg, opts.Suites)
	if err != nil {
		return nil, err
	}
	used := usedSandboxes(cfg, append(pairs, judges...))
	steps, err := scorerStepSandboxes(cfg, opts.Suites)
	if err != nil {
		return nil, err
	}
	maps.Copy(used, steps)
	want := map[string]bool{}
	for _, name := range opts.Sandboxes {
		if !used[name] {
			return nil, fmt.Errorf("--sandbox %q: no selected pair uses it", name)
		}
		want[name] = true
	}
	var out []config.Sandbox
	for _, sb := range cfg.Sandboxes {
		if !used[sb.Name] || (len(want) > 0 && !want[sb.Name]) || (opts.Driver != "" && sb.Driver != opts.Driver) {
			continue
		}
		out = append(out, sb)
	}
	if len(out) == 0 {
		return nil, errors.New("no sandbox to init: the selection uses none")
	}
	return out, nil
}

// judgePairsOf is a pair for each judge agent an agent scorer of the
// suites names: what a judge's sandbox is used by.
func judgePairsOf(cfg *config.Config, suites []string) ([]config.TestPair, error) {
	sps, err := cfg.ExpandScorePairs(suites...)
	if err != nil {
		return nil, err
	}
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.Name })
	seen := map[string]bool{}
	var out []config.TestPair
	for _, sp := range sps {
		if sc := scorers[sp.Scorer]; sc.IsAgent() && !seen[sc.Agent] {
			seen[sc.Agent] = true
			out = append(out, config.TestPair{Agent: sc.Agent})
		}
	}
	return out, nil
}

// initWhat says what init is about to do for sb, and where a build logs.
func initWhat(sb config.Sandbox, configDir string, placing bool) string {
	build := provisions(sb) || sb.Build != "" || placing
	if sb.Driver == sandbox.LXDDriver {
		build = sb.Image == ""
		if !build && !strings.Contains(sb.Image, ":") {
			return "pinning its image"
		}
	}
	if !build {
		return "pulling images"
	}
	return "building images (log: " + relToCwd(initLogPath(configDir, sb.Name)) + ")"
}

// initSandbox makes one sandbox's images and returns its pin. clis are the
// ones its agents run and pins what each is pinned at: an image pats
// composes gets each placed in it, and every image is asked for each and has
// to have it.
func initSandbox(ctx context.Context, sb config.Sandbox, clis []string, pins map[string]*machine.CLI, ro Options, opts InitOptions) (string, *pinEntry, error) {
	h, err := hostFor(sb)
	if err != nil {
		return "", nil, err
	}
	t, err := h.Target(ctx)
	if err != nil {
		return "", nil, err
	}
	inputs, err := localInputs(sb, ro.ConfigDir)
	if err != nil {
		return "", nil, err
	}
	e := &pinEntry{
		Daemon:      t.Daemon,
		PatsVersion: version.Info.Version,
		PatsBuild:   patsBuild(),
		Built:       opts.Now.UTC().Format(time.RFC3339),
		Inputs:      inputs,
	}
	if sb.Driver == sandbox.LXDDriver {
		err = initLXD(ctx, sb, clis, pins, ro, opts, h, e)
	} else {
		err = initContainer(ctx, sb, clis, pins, ro, opts, h, e)
	}
	if err == nil {
		// where the trees sit beside the image, a session of the two is
		// what an agent runs in, and what is asked.
		if th, ok := h.(sandbox.TreeHost); ok && len(e.Harnesses) > 0 {
			e.Components, err = th.StampsOver(ctx, e.Image.ID, treeIDs(e), clis)
		} else {
			e.Components, err = h.Stamps(ctx, e.Image.ID, clis)
		}
	}
	if err == nil {
		// an image: or a build: brings its own clis, and a provision can
		// remove one: either way the image is what says.
		var lacks []string
		for _, c := range clis {
			if _, ok := e.Components[c]; !ok {
				lacks = append(lacks, c)
			}
		}
		if len(lacks) > 0 {
			err = fmt.Errorf("its image has no %s on its PATH, which its agents run", strings.Join(lacks, ", "))
		}
	}
	if err == nil {
		upstreamInputs(ctx, sb, ro.ConfigDir, h, e)
	}
	return t.Key, e, err
}

// initContainer makes a docker or podman sandbox's image and proxy: built
// from build: or base / provision, else pulled when absent (always, with
// --force).
func initContainer(ctx context.Context, sb config.Sandbox, clis []string, pins map[string]*machine.CLI, ro Options, opts InitOptions, h builder, e *pinEntry) error {
	lg := logw{ro.Out, ro.Color, ro.Verbose}
	logPath := initLogPath(ro.ConfigDir, sb.Name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logF, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logF.Close()
	log := ro.buildLog(logF)
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
	progress := func(msg string) { lg.detail("sandbox %q: %s", sb.Name, msg) }
	var placed []sandbox.Placed
	var own *pinImage // pats's own sandbox image, once pulled
	if places(sb) && len(clis) > 0 {
		ref := sb.Base
		if ref == "" {
			ref = base()
		}
		// the base has to be there to say its arch. pats's own is pulled
		// afresh when forced, as it is when it is the sandbox's image.
		im, err := ensurePulled(ctx, h, ref, opts.Force && !provisions(sb), lg, log, h.Pull)
		if err != nil {
			return fail(fmt.Errorf("its base %s: %w", ref, err))
		}
		if !provisions(sb) {
			own = &im
		}
		if placed, err = placeIn(ctx, sb, ref, clis, pins, lg, h, e); err != nil {
			return fail(err)
		}
		// a place that builds no image keeps the trees beside the one it
		// pulls: nothing is built on it.
		if th, ok := h.(sandbox.TreeHost); ok {
			if _, err := th.PlaceTrees(ctx, placed); err != nil {
				return fail(err)
			}
			placed = nil
		}
	}
	froms := fromImages(sb, ro.ConfigDir)
	if len(placed) > 0 && !provisions(sb) {
		froms = []string{base()} // the pats sandbox, built on once it has a cli to place
	}
	// the build resolves its FROM tags itself, somewhere between these two
	// reads: a base the same on both is the one it used.
	before := baseDigests(ctx, froms, h, logF)
	defer func() {
		held, moved := stableBases(before, baseDigests(ctx, froms, h, logF))
		for ref, digest := range held {
			e.Inputs[upstreamPrefix+ref] = digest
		}
		for _, ref := range moved {
			lg.warn("sandbox %q: its base %s moved while init built on it; its upstream is not tracked until the next pats init", sb.Name, ref)
		}
	}()
	switch {
	// the default sandbox is built too once it has a cli to place: a pull of
	// the base and a copy on top.
	case provisions(sb) || len(placed) > 0:
		spec := sandbox.BuildSpec{Base: sb.Base, SandboxImage: base, Force: opts.Force, Progress: progress, Harness: placed}
		if sb.Provision != "" {
			material, argv, err := stageMaterial(ro.ConfigDir, sb.Provision, sandbox.ProvisionMount)
			if err != nil {
				return err
			}
			defer sandbox.RemoveTemp(material)
			spec.Material, spec.Argv = material, argv
		}
		img, _, _, err := h.Build(ctx, spec, log)
		if err != nil {
			return fail(err)
		}
		e.Image = pinImage{ID: img.ID, Built: true}
	case sb.Build != "":
		ctxDir, dockerfile, err := resolveBuildSpec(ro.ConfigDir, sb.Build)
		if err != nil {
			return err
		}
		img, _, _, err := h.Build(ctx, sandbox.BuildSpec{Dir: ctxDir, Dockerfile: dockerfile, SandboxImage: base, Force: opts.Force, Progress: progress}, log)
		if err != nil {
			return fail(err)
		}
		e.Image = pinImage{ID: img.ID, Built: true}
	case sb.Image != "":
		im, err := ensurePulled(ctx, h, sb.Image, opts.Force, lg, log, h.Pull)
		if err != nil {
			return err
		}
		e.Image = im
	case own != nil:
		e.Image = *own // its clis sit beside it
	default:
		im, err := ensurePulled(ctx, h, base(), opts.Force, lg, log, h.Pull)
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
		if !pinsProxy(sb) {
			// bwrap runs its proxy on the host: no image to pin, only the
			// in-sandbox forwarder to make present, so a run never fetches.
			if _, _, err := h.ProxyImage(ctx, ref, opts.Force, log); err != nil {
				return fail(fmt.Errorf("egress forwarder: %w", err))
			}
			return nil
		}
		im, err := ensurePulled(ctx, h, ref, opts.Force, lg, log, h.ProxyImage)
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		e.Proxy = &im
	}
	return nil
}

// placeIn readies what init places in a sandbox's image: the place is asked
// the arch of an image on base, each cli's artefact for it is fetched into
// the cache, checked, and the pin records the arch, what is placed and the
// input a run compares.
func placeIn(ctx context.Context, sb config.Sandbox, base string, clis []string, pins map[string]*machine.CLI, lg logw, h builder, e *pinEntry) ([]sandbox.Placed, error) {
	arch, err := h.Arch(ctx, base)
	if err != nil {
		return nil, err
	}
	arts, err := artefactsFor(pins, clis, arch)
	if err != nil {
		return nil, err
	}
	placed, rec, err := fetchPlaced(ctx, arts, lg, sb.Name)
	if err != nil {
		return nil, err
	}
	e.Arch, e.Harnesses = arch, rec
	e.Inputs["harnesses"] = harnessInput(arts)
	return placed, nil
}

// pullFunc is a host's Pull, or its ProxyImage.
type pullFunc func(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error)

// ensurePulled pins ref, pulling it with pull when absent or when forced.
// the image is pats's own only when this pull brought it: one already there,
// even a published pats image, may be the user's (a local `make images`
// build).
func ensurePulled(ctx context.Context, h puller, ref string, force bool, lg logw, log io.Writer, pull pullFunc) (pinImage, error) {
	// what the daemon held before: an image a pull lands on that was
	// already there, tagged or not, is not pats's.
	had, err := h.Images(ctx)
	if err != nil {
		return pinImage{}, err
	}
	if _, err := h.Resolve(ctx, ref); force || err != nil {
		lg.detail("pulling %s", ref)
	}
	img, fetched, err := pull(ctx, ref, force, log)
	if err != nil {
		return pinImage{}, err
	}
	return pinImage{ID: img.ID, Name: ref, Digest: img.Digest, Pulled: fetched && !had[img.ID]}, nil
}

// initLXD makes an lxd sandbox's layers and proxy on its server, logging to
// .pats/init/<sandbox>.log, and pins them by fingerprint.
func initLXD(ctx context.Context, sb config.Sandbox, clis []string, pins map[string]*machine.CLI, ro Options, opts InitOptions, h builder, e *pinEntry) error {
	lg := logw{ro.Out, ro.Color, ro.Verbose}
	switch {
	case strings.Contains(sb.Image, ":"):
		// an image of another remote: copied onto the server now, so a run
		// never downloads it.
		img, copied, err := h.Pull(ctx, sb.Image, opts.Force, nil)
		if err != nil {
			return err
		}
		e.Image = pinImage{ID: img.ID, Name: sb.Image, Pulled: copied}
	case sb.Image != "":
		img, err := h.Resolve(ctx, sb.Image)
		if err != nil {
			return err
		}
		e.Image = pinImage{ID: img.ID, Name: sb.Image}
	default:
		var placed []sandbox.Placed
		if len(clis) > 0 {
			var err error
			if placed, err = placeIn(ctx, sb, sb.Base, clis, pins, lg, h, e); err != nil {
				return err
			}
		}
		logPath := initLogPath(ro.ConfigDir, sb.Name)
		img, layers, _, upstream, err := lxdImage(ctx, h, sb, placed, clis, ro, logPath, opts.Force)
		if err != nil {
			return err
		}
		for ref, fp := range upstream {
			e.Inputs[upstreamPrefix+ref] = fp
		}
		e.Image = pinImage{ID: img.ID, Name: img.Name, Built: true}
		e.Layers = layers
	}
	if sb.Egress.Set {
		log := &stepLog{w: ro.buildLog(io.Discard),
			step: func(msg string) { lg.detail("sandbox %q: egress proxy: %s", sb.Name, msg) },
			warn: func(msg string) { lg.warn("sandbox %q: egress proxy: %s", sb.Name, msg) }}
		img, _, err := h.ProxyImage(ctx, "", opts.Force, log)
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		e.Proxy = &pinImage{ID: img.ID, Name: img.Name, Built: true}
	}
	return nil
}

// usePins points each sandbox the pairs use, and the extra ones (scorers'
// step sandboxes), at its pinned images, and stops the run, naming the init
// that fixes it, when one is not pinned for where the run would start it, or
// its image is gone.
func usePins(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair, extra map[string]bool) ([]upstreamCheck, error) {
	pins, err := readPins(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	var checks []upstreamCheck
	used := usedSandboxes(cfg, pairs)
	maps.Copy(used, extra)
	lg := logw{opts.Out, opts.Color, opts.Verbose}
	var missing []string
	var reasons []string
	for i, sb := range cfg.Sandboxes {
		if !used[sb.Name] {
			continue
		}
		if sandbox.TraitsOf(sb.Driver).NoImages {
			comps, err := hostComponents(ctx, sb, sandboxCLIs(cfg, sb.Name), cliPins(cfg, sb.Name), lg)
			if err != nil {
				return nil, err
			}
			cfg.Sandboxes[i].Components = comps
			continue
		}
		h, err := hostFor(sb)
		if err != nil {
			return nil, fmt.Errorf("sandbox %q: %w", sb.Name, err)
		}
		t, err := h.Target(ctx)
		if err != nil {
			return nil, fmt.Errorf("sandbox %q: %w", sb.Name, err)
		}
		target, daemon := t.Key, t.Daemon
		e := pins.entry(target, sb.Name)
		tree, goneTree := "", false
		if e != nil {
			tree, goneTree = missingTree(h, e)
		}
		reason := ""
		switch {
		case e == nil:
			reason = "not pinned for " + target
		case e.Daemon != daemon:
			reason = "pinned against another " + sb.Driver + " daemon"
		case !present(ctx, h, e.Image.ID):
			reason = "its pinned image " + short(e.Image.ID) + " is gone"
		case goneTree:
			reason = "a harness tree it was pinned with (" + short(tree) + ") is gone"
		case pinsProxy(sb) && e.Proxy == nil:
			reason = "no egress proxy pinned"
		case pinsProxy(sb) && !present(ctx, h, e.Proxy.ID):
			reason = "its pinned egress proxy " + short(e.Proxy.ID) + " is gone"
		case pinsProxy(sb):
			reason = proxyMismatch(e)
		}
		if reason != "" {
			missing = append(missing, sb.Name)
			reasons = append(reasons, fmt.Sprintf("  %s: %s", sb.Name, reason))
			continue
		}
		now, err := localInputs(sb, opts.ConfigDir)
		if err == nil {
			if in := harnessInputNow(cfg, sb, e.Arch); in != "" {
				now["harnesses"] = in
			}
		}
		if err != nil {
			lg.warn("sandbox %q: cannot tell whether its image is current: %v", sb.Name, err)
		} else if changed := staleLocal(e, now); len(changed) > 0 {
			lg.warn("sandbox %q: changed since its pats init: %s; running the pinned image (pats init --sandbox %s to rebuild)",
				sb.Name, strings.Join(changed, ", "), sb.Name)
		}
		if lacks := missingCLIs(cfg, pairs, sb.Name, e.Components); len(lacks) > 0 {
			missing = append(missing, sb.Name)
			reasons = append(reasons, fmt.Sprintf("  %s: its image was made without %s", sb.Name, strings.Join(lacks, ", ")))
			continue
		}
		if drift := staleComponents(e.Components, cliPins(cfg, sb.Name)); len(drift) > 0 {
			lg.warn("sandbox %q: its harnesses differ from what is pinned: %s (pats init --sandbox %s to rebuild)",
				sb.Name, strings.Join(drift, ", "), sb.Name)
		}
		cfg.Sandboxes[i].Image = e.Image.ID
		cfg.Sandboxes[i].Components = e.Components
		cfg.Sandboxes[i].Placed = e.Harnesses
		if _, ok := h.(sandbox.TreeHost); ok {
			cfg.Sandboxes[i].Trees = treeIDs(e)
		}
		checks = append(checks, upstreamCheck{sb: sb.Name, up: h.Upstream, inputs: e.Inputs})
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
		for _, name := range missing {
			cmd += " --sandbox " + name
		}
	}
	return nil, fmt.Errorf("images missing for %d sandbox(es):\n%s\nrun `%s` first", len(missing), strings.Join(reasons, "\n"), cmd)
}

// missingTree is a tree e records that its host, when it keeps trees beside
// its images, no longer has.
func missingTree(h imageHost, e *pinEntry) (id string, ok bool) {
	if th, is := h.(sandbox.TreeHost); is {
		return th.MissingTree(treeIDs(e))
	}
	return "", false
}

// missingImage is an image e pins that its host no longer has, "" when
// it has them all.
func missingImage(ctx context.Context, h imageHost, e *pinEntry) string {
	for _, im := range e.images() {
		if !present(ctx, h, im.ID) {
			return im.ID
		}
	}
	return ""
}

// present says whether h holds image ref.
func present(ctx context.Context, h imageHost, ref string) bool {
	_, err := h.Resolve(ctx, ref)
	return err == nil
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

// hostComponents are the harness versions on this machine's PATH, as a host
// sandbox's agents find them: what the run saw, not a guarantee of what each
// pair ran. a version other than this pats pins is a warning, never a
// refusal: host tools are the caller's.
func hostComponents(ctx context.Context, sb config.Sandbox, clis []string, pins map[string]string, lg logw) (map[string]string, error) {
	h, err := hostFor(sb)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", sb.Name, err)
	}
	comps, err := h.Stamps(ctx, "", clis)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", sb.Name, err)
	}
	if drift := staleComponents(comps, pins); len(drift) > 0 {
		lg.warn("sandbox %q: this machine's harnesses differ from what is pinned: %s", sb.Name, strings.Join(drift, ", "))
	}
	return comps, nil
}

// sandboxPins is the clis a sandbox's agents run, by name: those of every
// agent in the config that resolves to it, a judge among them, whatever a
// command selects, so an init or a run of part of the matrix sees the same
// image. each is its entry as the agent's harness resolves it, a project's
// own pin over the machine's; the config's load holds that two harnesses of
// one sandbox agree on it. a harness with no cli adds none.
func sandboxPins(cfg *config.Config, name string) map[string]*machine.CLI {
	out := map[string]*machine.CLI{}
	for _, a := range cfg.Agents {
		id, err := cfg.ResolveSandbox(a)
		if hd := cfg.HarnessOf(a); err == nil && id == name && hd != nil && hd.CLI != "" && out[hd.CLI] == nil {
			out[hd.CLI] = hd.Cli()
		}
	}
	return out
}

// sandboxCLIs is the names of sandboxPins, sorted.
func sandboxCLIs(cfg *config.Config, name string) []string {
	pins := sandboxPins(cfg, name)
	out := make([]string, 0, len(pins))
	for c := range pins {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// missingCLIs is the clis the selected pairs' agents run in sandbox name
// that its image's stamps lack: an agent added since the image was made,
// on a harness new to it.
func missingCLIs(cfg *config.Config, pairs []config.TestPair, name string, components map[string]string) []string {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	seen := map[string]bool{}
	var out []string
	for _, p := range pairs {
		a := agents[p.Agent]
		id, err := cfg.ResolveSandbox(a)
		hd := cfg.HarnessOf(a)
		if err != nil || id != name || hd == nil || hd.CLI == "" || seen[hd.CLI] {
			continue
		}
		seen[hd.CLI] = true
		if _, ok := components[hd.CLI]; !ok {
			out = append(out, hd.CLI)
		}
	}
	sort.Strings(out)
	return out
}

// cliPins is the version each cli of a sandbox is pinned at: what its
// stamps are compared with.
func cliPins(cfg *config.Config, name string) map[string]string {
	out := map[string]string{}
	for cli, c := range sandboxPins(cfg, name) {
		if c != nil {
			out[cli] = c.Version
		}
	}
	return out
}

// pinsProxy says whether a sandbox's egress proxy is an image init pins:
// docker's and podman's sidecar, lxd's proxy instance. bwrap and host run
// theirs on this machine.
func pinsProxy(sb config.Sandbox) bool {
	return sb.Egress.Set && (sb.IsContainer() || sb.Driver == sandbox.LXDDriver)
}
