//go:build !nobwrap

package sandbox

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/lczyk/pats/internal/version"
)

// the bwrap driver runs each sandbox with bubblewrap: unprivileged linux
// namespaces over an image's rootfs (bwrap_image.go), started by the invoking
// user, no daemon. each exec is one bwrap over the session's overlay, its
// upper dir shared by every exec of the session. egress under a policy goes
// through `pats egress forward` inside the sandbox's own net namespace,
// relaying to the proxy on the host over a unix socket. docs/spec/bwrap.md is
// the design.

// BwrapDriver is the bwrap driver's name.
const BwrapDriver = "bwrap"

// bwrapMin is the oldest bwrap with --overlay.
var bwrapMin = [2]int{0, 10}

type bwrapDriver struct {
	p    *process
	o    Options
	r    runner // bwrap itself
	arch string
	// pats is the binary the proxy runs from, and forwarder the static
	// linux pats bound into a sandbox; "" works them out (a test sets them).
	pats, forwarder string

	verOnce sync.Once
	verErr  error
}

func init() {
	registerDriver(BwrapDriver, driverDef{
		open: func(p *process, r runner, o Options) Driver {
			d := newBwrap(p, o)
			if r != nil {
				d.r = r
			}
			return d
		},
		reopen: reopenSame,
		// an unpack a crash cut short leaves <id>.new, taken only when no
		// unpack holds its lock.
		sweep: func(dryRun bool, report func(CleanEvent)) {
			st, err := newBwrapStore()
			if err != nil {
				return
			}
			for _, name := range st.sweepNew(dryRun) {
				report(CleanEvent{Action: "remove", Kind: "temp", Name: filepath.Join(st.dir, "rootfs", name)})
			}
		},
	})
}

func newBwrap(p *process, o Options) *bwrapDriver {
	return &bwrapDriver{p: p, o: o, r: cliRunner("bwrap"), arch: runtime.GOARCH}
}

func (d *bwrapDriver) store() (bwrapStore, error) { return newBwrapStore() }

// usable is the start's verdict on this machine: linux, and a bwrap new
// enough for --overlay. the rest of what bwrap needs shows as its own error
// on the first exec.
func (d *bwrapDriver) usable(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the bwrap driver runs on linux only (this is %s)", runtime.GOOS)
	}
	d.verOnce.Do(func() {
		out, err := combined(ctx, d.r, "--version")
		if err != nil {
			d.verErr = fmt.Errorf("bwrap %d.%d or later is needed on PATH: %v %s", bwrapMin[0], bwrapMin[1], err, out)
			return
		}
		d.verErr = bwrapVersionOK(out)
	})
	return d.verErr
}

var bwrapVersionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// bwrapVersionOK reads `bwrap --version` and refuses one older than bwrapMin.
func bwrapVersionOK(out string) error {
	m := bwrapVersionRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("bwrap --version said %q, no version in it", strings.TrimSpace(out))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < bwrapMin[0] || (major == bwrapMin[0] && minor < bwrapMin[1]) {
		return fmt.Errorf("bwrap %s is too old: %d.%d or later is needed, for --overlay", m[0], bwrapMin[0], bwrapMin[1])
	}
	return nil
}

func (d *bwrapDriver) Target(context.Context) (Target, error) {
	host, _ := os.Hostname()
	return Target{Key: BwrapDriver + ":" + host}, nil
}

func (d *bwrapDriver) CPUs(context.Context) int { return runtime.NumCPU() }

func (d *bwrapDriver) Resolve(_ context.Context, ref string) (Image, error) {
	st, err := d.store()
	if err != nil {
		return Image{}, err
	}
	if strings.HasPrefix(ref, "sha256:") {
		if st.has(ref) {
			return Image{ID: ref}, nil
		}
		return Image{}, fmt.Errorf("%w: %s", ErrNoImage, ref)
	}
	id, repoDigest, ok := st.resolveRef(ref)
	if !ok {
		return Image{}, fmt.Errorf("%w: %s", ErrNoImage, ref)
	}
	repo, digest, _ := strings.Cut(repoDigest, "@")
	return Image{ID: id, Name: ref, Digest: digest, Repo: repo}, nil
}

func (d *bwrapDriver) Images(context.Context) (map[string]bool, error) {
	st, err := d.store()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range st.ids() {
		out[id] = true
	}
	return out, nil
}

// Pull fetches ref for this machine's arch and unpacks it into the store.
func (d *bwrapDriver) Pull(ctx context.Context, ref string, force bool, log io.Writer) (Image, bool, error) {
	st, err := d.store()
	if err != nil {
		return Image{}, false, err
	}
	ri, err := d.p.reg.image(ctx, ref, d.arch)
	if err != nil {
		return Image{}, false, err
	}
	fetched, err := st.pull(ctx, ri, force, func(msg string) {
		if log != nil {
			fmt.Fprintln(log, msg)
		}
	})
	if err != nil {
		return Image{}, false, err
	}
	return Image{ID: ri.manifest, Name: ref, Digest: ri.digest, Repo: ri.name}, fetched, nil
}

func (d *bwrapDriver) Build(context.Context, BuildSpec, io.Writer) (Image, string, bool, error) {
	return Image{}, "", false, errors.New("the bwrap driver builds no images: give it an image ref")
}

// ProxyImage makes the forwarder's binary present: there is no proxy image,
// the proxy runs on the host. ref is the published proxy image a non-static
// pats takes the binary from.
func (d *bwrapDriver) ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (Image, bool, error) {
	if self := staticSelf(d.arch); self != "" {
		return Image{ID: self}, false, nil
	}
	st, err := d.store()
	if err != nil {
		return Image{}, false, err
	}
	dst := st.forwarderPath(d.arch)
	if _, err := os.Stat(dst); err == nil && !force {
		return Image{ID: dst}, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return Image{}, false, err
	}
	if log != nil {
		fmt.Fprintf(log, "fetching the egress forwarder for linux/%s from %s\n", d.arch, ref)
	}
	if err := d.p.reg.fetchImageFile(ctx, ref, d.arch, "pats", dst); err != nil {
		return Image{}, false, fmt.Errorf("fetch the egress forwarder from %s: %w", ref, err)
	}
	return Image{ID: dst}, true, nil
}

// forwarderPath is where init keeps the forwarder for this pats version.
func (st bwrapStore) forwarderPath(arch string) string {
	return filepath.Join(st.dir, "pats-"+version.Info.Version+"-"+arch)
}

// staticSelf is this pats when it is a static linux binary for arch (no ELF
// interpreter), "" otherwise.
func staticSelf(arch string) string {
	if runtime.GOOS != "linux" || runtime.GOARCH != arch {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := elf.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return ""
		}
	}
	return exe
}

// forwarderBin is the forwarder a run binds in: this pats if static, else
// the one init fetched. a run never fetches.
func (d *bwrapDriver) forwarderBin() (string, error) {
	if d.forwarder != "" {
		return d.forwarder, nil
	}
	if self := staticSelf(d.arch); self != "" {
		return self, nil
	}
	st, err := d.store()
	if err != nil {
		return "", err
	}
	p := st.forwarderPath(d.arch)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("egress: no forwarder for linux/%s (%s); run pats init", d.arch, p)
	}
	return p, nil
}

// Stamps are the versions of clis in image id: read from beside it when what
// is kept there answers for each, or found by stampScript in a throwaway
// session and kept there.
func (d *bwrapDriver) Stamps(ctx context.Context, id string, clis []string) (map[string]string, error) {
	if len(clis) == 0 {
		return map[string]string{}, nil
	}
	st, err := d.store()
	if err != nil {
		return nil, err
	}
	kept := map[string]string{}
	if b, err := os.ReadFile(st.stampsFile(id)); err == nil && json.Unmarshal(b, &kept) == nil && covers(kept, clis) {
		return wanted(kept, clis), nil
	}
	work, err := d.p.mkTemp("pats-stamp-")
	if err != nil {
		return nil, err
	}
	defer RemoveTemp(work)
	sess, err := (&bwrapSandbox{d: d, image: id}).Start(ctx, Spec{Workdir: work, Egress: Egress{None: true}})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	rctx, cancel := context.WithTimeout(ctx, d.p.t.stamp)
	defer cancel()
	var out, errb bytes.Buffer
	code, err := sess.Exec(rctx, Cmd{Argv: []string{"/bin/sh", "-c", stampScript(clis)}, Root: true, Dir: "/"}, &out, &errb)
	switch {
	case err != nil:
		return nil, err
	case code == 127 || code == 126:
		return map[string]string{}, nil // no shell: no stamps
	case code != 0:
		return nil, fmt.Errorf("stamps: exited %d: %s", code, strings.TrimSpace(errb.String()))
	}
	stamps, err := parseStamps(out.String())
	if err != nil {
		return nil, err
	}
	// what was kept for other clis stays: another project's set.
	if kept == nil {
		kept = map[string]string{}
	}
	for k, v := range stamps {
		kept[k] = v
	}
	b, _ := json.Marshal(kept)
	_ = os.WriteFile(st.stampsFile(id), b, 0o600)
	return stamps, nil
}

func (d *bwrapDriver) RemoveImage(_ context.Context, id string) error {
	st, err := d.store()
	if err != nil {
		return err
	}
	return st.remove(id)
}

// Arch is this machine's: the one its rootfs was fetched for.
func (d *bwrapDriver) Arch(context.Context, string) (string, error) { return placeArch(d.arch) }

func (d *bwrapDriver) Upstream(ctx context.Context, ref string) (string, error) {
	return d.p.reg.tagDigest(ctx, ref)
}

// Runs lists nothing: nothing of a run outlives pats but files, which clean
// sweeps as temp dirs.
func (d *bwrapDriver) Runs(context.Context) ([]RunRecord, error) { return nil, nil }
func (d *bwrapDriver) Remove(context.Context, Resource) error    { return nil }

func (d *bwrapDriver) Sandbox(o SandboxOptions) (Sandbox, error) {
	if o.Image == "" {
		return nil, errors.New("sandbox driver bwrap needs an image")
	}
	return &bwrapSandbox{d: d, image: o.Image}, nil
}

// bwrapSandbox makes sessions of one image.
type bwrapSandbox struct {
	d     *bwrapDriver
	image string // a store id, or a ref init pulled
}

func (b *bwrapSandbox) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	return run(ctx, b, spec, stdout, stderr)
}

// the in-sandbox paths of what pats binds in.
const (
	bwrapEgressDir = "/run/pats-egress"
	bwrapPats      = "/run/pats/pats"
	bwrapAptConf   = "/run/pats/apt.conf"
	bwrapStaging   = "/run/pats-push"
	bwrapListen    = "127.0.0.1:8080"
)

// netMode is a session's network: open (the host's), none (its own, empty),
// or proxy (its own, out only through the forwarder).
type netMode int

const (
	netOpen netMode = iota
	netNone
	netProxy
)

func (b *bwrapSandbox) Start(ctx context.Context, spec Spec) (Session, error) {
	d := b.d
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if err := d.usable(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if err := d.o.machineErr; err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workdir: %w", ErrLaunch, err)
	}
	if err := d.p.holdLease(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	st, err := d.store()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	id := b.image
	if !strings.HasPrefix(id, "sha256:") {
		img, err := d.Resolve(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%w: image %s: %w (pats init pulls it)", ErrLaunch, id, err)
		}
		id = img.ID
	}
	unlock, err := st.lock(ctx, id, false, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if !st.has(id) {
		unlock()
		return nil, fmt.Errorf("%w: image %s is not in the store; run pats init", ErrLaunch, short12(id))
	}
	s := &bwrapSession{d: d, rootfs: st.rootfs(id), workdir: abs, unlock: unlock}
	s.imageEnv, _ = st.imageEnv(id)
	fail := func(err error) (Session, error) {
		s.Close()
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if s.dir, err = b.d.p.mkTemp("pats-bwrap-"); err != nil {
		return fail(err)
	}
	for _, sub := range []string{"upper", "work", "egress"} {
		if err := os.MkdirAll(filepath.Join(s.dir, sub), 0o700); err != nil {
			return fail(err)
		}
	}
	// apt's fetchers drop to _apt, a uid the namespace does not have.
	if err := os.WriteFile(filepath.Join(s.dir, "apt.conf"), []byte("APT::Sandbox::User \"root\";\n"), 0o644); err != nil {
		return fail(err)
	}
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			return fail(err)
		}
		s.binds = append(s.binds, bind{host: mh, path: m.Container, ro: m.ReadOnly, masks: m.Masks})
	}
	switch {
	case spec.Egress.None:
		s.net = netNone
	case spec.Egress.Set:
		s.net = netProxy
		if err := s.startProxy(ctx, spec.Egress); err != nil {
			return fail(err)
		}
	default:
		s.net = netOpen
		if err := s.openNetwork(ctx, spec.Egress.SelfTest); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

type bwrapSession struct {
	d        *bwrapDriver
	rootfs   string
	dir      string // the session's: upper/, work/, egress/, apt.conf
	workdir  string
	binds    []bind
	imageEnv []string
	env      map[string]string // under every exec's own: the egress or machine proxy env
	net      netMode
	trust    *material // the merged bundle, bound at /sbx-ca
	unlock   func()

	proxy *proxyChild

	mu      sync.Mutex // one exec at a time: two overlays may not share an upper dir
	closeMu sync.Mutex
	closed  bool
	cancel  context.CancelFunc // the exec in flight
	once    sync.Once
}

// argv is the bwrap command line for c: no env value is on it, only names
// of paths.
func (s *bwrapSession) argv(c Cmd, extra ...string) []string {
	a := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts"}
	if s.net != netOpen {
		a = append(a, "--unshare-net")
	}
	a = append(a, "--die-with-parent", "--new-session",
		"--overlay-src", s.rootfs, "--overlay", filepath.Join(s.dir, "upper"), filepath.Join(s.dir, "work"), "/",
		"--proc", "/proc", "--dev", "/dev",
		"--bind", s.workdir, WorkMount)
	for _, b := range s.binds {
		flag := "--bind"
		if b.ro {
			flag = "--ro-bind"
		}
		a = append(a, flag, b.host, b.path)
		for _, m := range b.masks {
			a = append(a, "--tmpfs", m)
		}
	}
	if s.trust != nil && s.trust.bundlePath != "" {
		a = append(a, "--ro-bind", s.trust.bundlePath, agentBundlePath, "--ro-bind", s.trust.certPath, agentCertPath)
	}
	if s.net == netProxy {
		a = append(a, "--bind", filepath.Join(s.dir, "egress"), bwrapEgressDir, "--ro-bind", s.proxy.forwarder, bwrapPats)
	}
	if c.Root {
		a = append(a, "--ro-bind", filepath.Join(s.dir, "apt.conf"), bwrapAptConf, "--uid", "0", "--gid", "0", "--cap-add", "ALL")
	} else {
		a = append(a, "--uid", strconv.Itoa(os.Getuid()), "--gid", strconv.Itoa(os.Getgid()))
	}
	a = append(a, extra...)
	dir := c.Dir
	if dir == "" {
		dir = WorkMount
	}
	a = append(a, "--clearenv", "--chdir", dir, "--")
	if s.net == netProxy {
		a = append(a, bwrapPats, "egress", "forward", "--listen", bwrapListen, "--socket", bwrapEgressDir+"/proxy.sock", "--")
	}
	return append(a, envArgv(c.Argv)...)
}

// execEnv is what a command gets: the image's Env, the session's, then its
// own; root gets apt told to stay root.
func (s *bwrapSession) execEnv(c Cmd) map[string]string {
	env := map[string]string{}
	for _, kv := range s.imageEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if env["PATH"] == "" {
		env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	if c.Root {
		env["APT_CONFIG"] = bwrapAptConf
		env["HOME"] = "/root"
	}
	return mergeEnv(mergeEnv(env, s.env), c.Env)
}

func (s *bwrapSession) Exec(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error) {
	return s.exec(ctx, c, stdout, stderr)
}

func (s *bwrapSession) exec(ctx context.Context, c Cmd, stdout, stderr io.Writer, extra ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// a context that ended while another exec held the session never starts.
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	xctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return -1, fmt.Errorf("%w: the session is closed", ErrLaunch)
	}
	s.cancel = cancel
	s.closeMu.Unlock()
	defer func() {
		s.closeMu.Lock()
		s.cancel = nil
		s.closeMu.Unlock()
	}()
	x, err := s.d.r.Start(envScript(s.execEnv(c)), stdout, stderr, s.argv(c, extra...)...)
	if err != nil {
		return -1, fmt.Errorf("%w: bwrap: %w", ErrLaunch, err)
	}
	done := make(chan error, 1)
	go func() { done <- x.Wait() }()
	var werr error
	killed := false
	select {
	case werr = <-done:
	case <-xctx.Done():
		select {
		case werr = <-done: // it finished as the context ended
		default:
			// bwrap is pid 1 of the sandbox's pid namespace: everything
			// inside ends with it.
			_ = x.Signal(os.Kill)
			werr = <-done
			killed = true
		}
	}
	code, err := exitResult(werr, "bwrap")
	// as execCancellable reads a cancelled exec: a bwrap that finished as the
	// context ended keeps its result, a killed one's status is the result,
	// and one that exited clean all the same reports the cancel.
	switch {
	case !killed || err != nil:
		return code, err
	case werr == nil:
		return -1, fmt.Errorf("bwrap: %w", xctx.Err())
	}
	return -1, nil
}

// Push copies a host file, or a directory's contents, to path inside, as
// root: the host side bound read-only at a staging path and copied, so what
// lands is an ordinary file of the overlay.
func (s *bwrapSession) Push(ctx context.Context, hostPath, path string) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	script := `mkdir -p "$(dirname "$2")" && cp -a "$1" "$2"`
	if fi.IsDir() {
		script = `mkdir -p "$2" && cp -a "$1/." "$2"`
	}
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: []string{"/bin/sh", "-c", script, "push", bwrapStaging, path}, Root: true, Dir: "/"},
		&out, &out, "--ro-bind", hostPath, bwrapStaging)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("bwrap push %s: %w", hostPath, err)
	}
	return nil
}

// Pull copies a bound path's contents from its host side; any other path
// has no host side to read.
func (s *bwrapSession) Pull(_ context.Context, path, hostDir string) error {
	all := append([]bind{{host: s.workdir, path: WorkMount}}, s.binds...)
	for _, b := range all {
		rel, err := filepath.Rel(b.path, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return copyUnder(b.host, rel, hostDir)
		}
	}
	return fmt.Errorf("bwrap pull %s: only a bound path can be pulled", path)
}

func (s *bwrapSession) HandOver(context.Context, ...string) error { return nil }
func (s *bwrapSession) Sync(context.Context) error                { return nil }

// Close kills a running exec, stops the proxy, releases the image and
// removes the session dir; safe to call twice.
func (s *bwrapSession) Close() error {
	s.once.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		if s.cancel != nil {
			s.cancel()
		}
		s.closeMu.Unlock()
		s.mu.Lock() // the exec in flight has ended
		s.mu.Unlock()
		if s.proxy != nil {
			s.proxy.stop()
		}
		if s.trust != nil {
			s.trust.cleanup()
		}
		if s.dir != "" {
			_ = removeTree(s.dir)
		}
		if s.unlock != nil {
			s.unlock()
		}
	})
	return nil
}

// openNetwork: the host's network, with the host's resolver written over
// the image's (the sandbox shares the host's loopback, where a stub resolver
// may listen); behind a machine proxy its env and trust; and the self-test.
func (s *bwrapSession) openNetwork(ctx context.Context, selfTest bool) error {
	if b, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		etc := filepath.Join(s.dir, "upper", "etc")
		if err := os.MkdirAll(etc, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), b, 0o644); err != nil {
			return err
		}
	}
	mp := s.d.o.Machine
	if c := mp.chain(); c != nil {
		s.env = c.AgentEnv()
		if mp.trust() != nil {
			m, err := s.d.p.prepare(Egress{}, mp.trust(), s.readBundle)
			if err != nil {
				return err
			}
			s.trust = m
			s.env = mergeEnv(s.env, m.trustEnv())
		}
	}
	if !selfTest {
		return nil
	}
	if c := mp.chain(); c != nil {
		for _, u := range c.Upstreams() {
			var out bytes.Buffer
			_, _ = s.exec(ctx, Cmd{Argv: probeReach.argv(u.Hostname(), u.Port()), Root: true, Dir: "/"}, &out, &out)
			if reached, _ := reachVerdict(out.String()); !reached {
				return fmt.Errorf("bwrap: the sandbox cannot reach the upstream %s", redactURL(u.String()))
			}
		}
		return nil
	}
	var out bytes.Buffer
	if code, err := s.exec(ctx, Cmd{Argv: probeOnline.argv(), Root: true, Dir: "/"}, &out, &out); err != nil || code != 0 {
		return fmt.Errorf("bwrap: an open sandbox does not reach the internet: %v %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// readBundle is the image's trust bundle, read off the rootfs.
func (s *bwrapSession) readBundle() ([]byte, error) {
	p, err := inRootfs(s.rootfs, caBundlePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// inRootfs resolves p inside rootfs, following symlinks as the sandbox
// would (an absolute target from the rootfs's root), never out of it.
func inRootfs(rootfs, p string) (string, error) {
	cur := "/"
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for hops := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(filepath.Join(rootfs, next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if hops++; hops > 40 {
				return "", fmt.Errorf("%s: too many links", p)
			}
			t, err := os.Readlink(filepath.Join(rootfs, next))
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(t) {
				cur = "/"
			}
			parts = append(strings.Split(strings.Trim(t, "/"), "/"), parts...)
			continue
		}
		cur = next
	}
	return filepath.Join(rootfs, cur), nil
}

// startProxy runs the proxy on the host, on a unix socket in the session's
// egress dir, and hands the agent the forwarder's address; the self-test
// proves the namespace has no other way out.
func (s *bwrapSession) startProxy(ctx context.Context, eg Egress) error {
	fwd, err := s.d.forwarderBin()
	if err != nil {
		return err
	}
	m, err := s.d.p.prepare(eg, s.d.o.Machine.trust(), s.readBundle)
	if err != nil {
		return err
	}
	s.trust = m
	penv := m.proxyEnv(m.bundlePath)
	penv["PATS_EGRESS_LISTEN"] = "unix:" + filepath.Join(s.dir, "egress", "proxy.sock")
	if s.proxy, err = startProxyChild(ctx, s.d.p, s.d.pats, penv, eg.AuditPath); err != nil {
		return err
	}
	s.proxy.forwarder = fwd
	s.env = m.agentEnv("http://" + bwrapListen)
	if !eg.SelfTest {
		return nil
	}
	var out bytes.Buffer
	_, _ = s.exec(ctx, Cmd{Argv: probeCanary.argv(), Root: true, Dir: "/", Env: map[string]string{
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "http_proxy": "", "https_proxy": "",
	}}, &out, &out)
	if err := canaryVerdict(out.String(), BwrapDriver, s.rootfs); err != nil {
		return err
	}
	out.Reset()
	_, _ = s.exec(ctx, Cmd{Argv: probeReach.argv("127.0.0.1", "8080"), Root: true, Dir: "/"}, &out, &out)
	if reached, _ := reachVerdict(out.String()); !reached {
		return fmt.Errorf("egress self-test: the sandbox cannot reach its forwarder at %s: %s", bwrapListen, strings.TrimSpace(out.String()))
	}
	return nil
}

// redactURL masks a url's password.
func redactURL(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		if j := strings.IndexByte(s[i+3:], '@'); j >= 0 {
			cred := s[i+3 : i+3+j]
			if u, _, ok := strings.Cut(cred, ":"); ok {
				return s[:i+3] + u + ":xxxxx" + s[i+3+j:]
			}
		}
	}
	return s
}
