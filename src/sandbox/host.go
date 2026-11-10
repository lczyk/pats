package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// the host driver runs a sandbox's commands as plain processes on this
// machine, as the invoking user, with that user's access to everything: no
// image, no daemon, no network of its own. it is dangerous by design (see
// docs/spec/host-driver.md): the config key sand-not-boxed says so. what it
// does promise is ordinary execution, each command in a process group of its
// own that ends with it, and an honest record. egress under it is advisory:
// the proxy runs as a child process and the agent is handed its env, but
// nothing stops a process that ignores that env.

// HostDriver is the driver name of the host driver.
const HostDriver = "host"

// host is the host driver: one per pats process, bound to nothing.
type host struct {
	p *process
	o Options
	// pats is the binary the egress proxy runs from: this one, but a test's
	// binary is not pats.
	pats string
}

func newHost(p *process, o Options) *host {
	// the agent runs in a workdir of its own: a relative SSL_CERT_FILE
	// would name nothing there.
	if o.TrustFile != "" {
		if abs, err := filepath.Abs(o.TrustFile); err == nil {
			o.TrustFile = abs
		}
	}
	return &host{p: p, o: o}
}

var errHostNoImages = errors.New("the host driver has no images")

func (h *host) Target(context.Context) (Target, error) { return Target{Key: HostDriver}, nil }
func (h *host) CPUs(context.Context) int               { return runtime.NumCPU() }
func (h *host) Resolve(context.Context, string) (Image, error) {
	return Image{}, fmt.Errorf("%w: %w", ErrNoImage, errHostNoImages)
}
func (h *host) Images(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
func (h *host) Pull(context.Context, string, bool, io.Writer) (Image, bool, error) {
	return Image{}, false, errHostNoImages
}
func (h *host) Build(context.Context, BuildSpec, io.Writer) (Image, string, bool, error) {
	return Image{}, "", false, errHostNoImages
}
func (h *host) ProxyImage(context.Context, string, bool, io.Writer) (Image, bool, error) {
	return Image{}, false, errHostNoImages
}
func (h *host) RemoveImage(context.Context, string) error        { return nil }
func (h *host) Upstream(context.Context, string) (string, error) { return "", nil }
func (h *host) Runs(context.Context) ([]RunRecord, error)        { return nil, nil }
func (h *host) Remove(context.Context, Resource) error           { return nil }
func (h *host) Sandbox(SandboxOptions) (Sandbox, error)          { return &hostSandbox{h: h}, nil }
func (h *host) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	return run(ctx, &hostSandbox{h: h}, spec, stdout, stderr)
}

// Stamps are the harness clis on this machine's PATH (the agent's, as
// hostEnv gives it), by stampScript; id is ignored, there is no image.
func (h *host) Stamps(ctx context.Context, _ string) (map[string]string, error) {
	rctx, cancel := context.WithTimeout(ctx, stampTimeout)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", stampScript)
	cmd.Env = hostEnv(nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := RunGroup(rctx, cmd); err != nil {
		return nil, fmt.Errorf("harness versions on this machine: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseStamps(out.String())
}

// hostPassed are the names an agent under host takes from pats's own env:
// what a cli needs to be found and to talk (PATH, the locale, the terminal,
// the temp dir). nothing else is inherited.
var hostPassed = map[string]bool{"LANG": true, "TERM": true, "TMPDIR": true}

// hostEnv is the environment a host command runs with: the passed names from
// pats's env, PATH without relative or empty entries (so the cli preflight
// finds is the one every pair runs), then env over them, one entry a name.
func hostEnv(env map[string]string) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH":
			var keep []string
			for _, d := range filepath.SplitList(v) {
				if d != "" && filepath.IsAbs(d) {
					keep = append(keep, d)
				}
			}
			m["PATH"] = strings.Join(keep, string(os.PathListSeparator))
		case hostPassed[k], strings.HasPrefix(k, "LC_"):
			m[k] = v
		}
	}
	return sortedEnv(mergeEnv(m, env))
}

// lookIn finds name on path as exec.LookPath would on that PATH, not
// pats's own; a name with a slash is taken as it is.
func lookIn(path, name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on the agent's PATH", name)
}

// envValue is key's value in an environ list.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// HostLookPath finds a cli as a host agent would: on pats's PATH less its
// relative and empty entries.
func HostLookPath(name string) (string, error) {
	return lookIn(envValue(hostEnv(nil), "PATH"), name)
}

// hostSandbox makes host sessions.
type hostSandbox struct{ h *host }

func (b *hostSandbox) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	return run(ctx, b, spec, stdout, stderr)
}

// paths: under host, the dirs themselves. prepare's material sits in the
// harness home, from where the prepare step removes it before the agent runs.
func (b *hostSandbox) paths(workdir, home string) Paths {
	return Paths{Work: workdir, Home: home, Prepare: filepath.Join(home, ".pats-prepare")}
}

// Start holds the lease and, with an egress policy, starts the proxy. a host
// sandbox with no policy hands its agent the machine's proxy env, as pats
// itself got it, so a machine that only gets out through a proxy still does.
func (b *hostSandbox) Start(ctx context.Context, spec Spec) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workdir: %w", ErrLaunch, err)
	}
	if err := b.h.p.holdLease(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if err := b.h.o.machineCheckHost(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	s := &hostSession{b: b, workdir: abs}
	switch {
	case spec.Egress.Set:
		if err := s.startProxy(ctx, spec.Egress); err != nil {
			s.Close()
			return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
	case !spec.Egress.None:
		s.env = b.h.o.Machine.hostPassthrough(b.h.o.TrustFile)
	}
	return s, nil
}

// machineCheckHost is machineCheck without the loopback refusal: a host
// agent reaches the host's loopback as pats does.
func (o Options) machineCheckHost() error { return o.machineErr }

// hostPassthrough is the machine's proxy env as an open host agent gets it,
// with the trust env pointing at SSL_CERT_FILE when it is set. nil without a
// machine proxy.
func (m *MachineProxy) hostPassthrough(trustFile string) map[string]string {
	if m.chain() == nil {
		return nil
	}
	env := m.Chain.AgentEnv()
	if trustFile != "" {
		env = mergeEnv(env, trustEnvAt(trustFile, trustFile))
	}
	return env
}

// trustEnvAt is the trust env for a bundle and the added certificates at
// these paths.
func trustEnvAt(bundle, added string) map[string]string {
	return map[string]string{
		"SSL_CERT_FILE":       bundle,
		"CURL_CA_BUNDLE":      bundle,
		"GIT_SSL_CAINFO":      bundle,
		"REQUESTS_CA_BUNDLE":  bundle,
		"CARGO_HTTP_CAINFO":   bundle,
		"NODE_EXTRA_CA_CERTS": added,
	}
}

// hostSession is one host sandbox: its workdir, the env every command gets
// on top of its own, and the egress proxy child when there is a policy.
type hostSession struct {
	b       *hostSandbox
	workdir string
	env     map[string]string

	proxy *proxyChild
	caDir string // the run CA and merged bundle, "" without tls inspection
	once  sync.Once

	mu      sync.Mutex
	closed  bool
	running map[*int]context.CancelFunc // the execs in flight, ended by close
	execs   sync.WaitGroup
}

// track registers an exec's cancel with the session and returns its handle,
// nil once the session is closed.
func (s *hostSession) track(cancel context.CancelFunc) *int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.running == nil {
		s.running = map[*int]context.CancelFunc{}
	}
	id := new(int)
	s.running[id] = cancel
	s.execs.Add(1)
	return id
}

func (s *hostSession) untrack(id *int) {
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
	s.execs.Done()
}

// startProxy runs `pats egress run` as a child on 127.0.0.1:0 and hands the
// session the proxy env. tls inspection needs a bundle the user vouches for
// (SSL_CERT_FILE): pats builds none from the system and never touches the
// system's trust.
func (s *hostSession) startProxy(ctx context.Context, eg Egress) error {
	o := s.b.h.o
	penv := map[string]string{}
	var bundle, added string
	if eg.Policy.NeedsCA() {
		if o.TrustFile == "" {
			return errors.New("egress: tls inspection (a url entry or methods) under the host driver needs SSL_CERT_FILE set to a trust bundle you vouch for; pats builds none from the system")
		}
		roots, err := os.ReadFile(o.TrustFile)
		if err != nil {
			return fmt.Errorf("egress: SSL_CERT_FILE: %w", err)
		}
		m, err := s.b.h.p.prepare(eg, nil, func() ([]byte, error) { return roots, nil })
		if err != nil {
			return err
		}
		s.caDir, bundle, added = m.caDir, m.bundlePath, m.certPath
		penv = mergeEnv(penv, m.proxyEnv(o.TrustFile))
	} else {
		m, err := s.b.h.p.prepare(eg, nil, nil)
		if err != nil {
			return err
		}
		penv = mergeEnv(penv, m.proxyEnv(""))
		// no run CA: an upstream that inspects tls still needs the
		// machine's trust, which the agent's env would otherwise lose.
		bundle, added = o.TrustFile, o.TrustFile
	}
	pc, err := startProxyChild(ctx, s.b.h.pats, mergeEnv(penv, map[string]string{"PATS_EGRESS_LISTEN": "127.0.0.1:0"}), eg.AuditPath)
	if err != nil {
		return err
	}
	s.proxy = pc
	listen := pc.addr
	url := "http://" + listen
	s.env = map[string]string{
		"HTTP_PROXY": url, "HTTPS_PROXY": url, "http_proxy": url, "https_proxy": url,
		"NO_PROXY": "", "no_proxy": "", "ALL_PROXY": "", "all_proxy": "",
	}
	if bundle != "" {
		s.env = mergeEnv(s.env, trustEnvAt(bundle, added))
	}
	return nil
}

// errHostProxyDied is a pair whose proxy ended while the agent ran.
var errHostProxyDied = errors.New("egress: the proxy exited while the command ran")

func (s *hostSession) Exec(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if len(c.Argv) == 0 {
		return -1, errors.New("host exec: no command")
	}
	env := hostEnv(mergeEnv(s.env, c.Env))
	bin, err := lookIn(envValue(env, "PATH"), c.Argv[0])
	if err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	cmd := exec.Command(bin, c.Argv[1:]...)
	cmd.Args[0] = c.Argv[0]
	cmd.Dir = c.Dir
	if cmd.Dir == "" {
		cmd.Dir = s.workdir
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// close ends what still runs: the exec's context is the session's too.
	xctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id := s.track(cancel)
	if id == nil {
		return -1, fmt.Errorf("%w: the session is closed", ErrLaunch)
	}
	defer s.untrack(id)
	// a proxy that dies while the command runs fails it at once: waiting
	// for the deadline would make it a timeout, or never end.
	var proxyDied atomic.Bool
	if s.proxy != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-s.proxy.done:
				proxyDied.Store(true)
				cancel()
			case <-stop:
			}
		}()
	}
	err = RunGroup(xctx, cmd)
	if cmd.Process == nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	code, err := exitResult(err, "host exec")
	if s.proxy != nil {
		select {
		case <-s.proxy.done:
			proxyDied.Store(true)
		default:
		}
	}
	if proxyDied.Load() && ctx.Err() == nil {
		return code, errHostProxyDied
	}
	return code, err
}

// Push copies a host file to path, or a directory's contents into path.
func (s *hostSession) Push(_ context.Context, hostPath, path string) error {
	return copyInto(hostPath, path)
}

// Pull copies the contents of the directory path into hostDir.
func (s *hostSession) Pull(_ context.Context, path, hostDir string) error {
	return copyInto(path, hostDir)
}

func (s *hostSession) HandOver(context.Context, ...string) error { return nil }
func (s *hostSession) Sync(context.Context) error                { return nil }

// Close ends the execs still running and waits for them, stops and reaps
// the proxy, and removes the CA material; it is safe to call twice.
func (s *hostSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		for _, cancel := range s.running {
			cancel()
		}
		s.mu.Unlock()
		s.execs.Wait()
		if s.proxy != nil {
			s.proxy.stop()
		}
		if s.caDir != "" {
			_ = RemoveTemp(s.caDir)
		}
	})
	return nil
}

// copyInto copies src (a file, or a directory's contents) into dst, keeping
// modes and links. src and dst the same path is nothing to do.
func copyInto(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyHostFile(src, dst, fi.Mode().Perm())
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyHostFile(p, target, info.Mode().Perm())
		}
		return nil
	})
}

func copyHostFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Paths are the paths pats names to an agent: its workdir (and cwd), its
// HOME, and where prepare's material lives while prepare runs.
type Paths struct {
	Work, Home, Prepare string
}

// the in-sandbox paths every driver but host binds or pushes to.
const (
	HomeMount    = "/pats-home"
	PrepareMount = "/pats-prepare"
)

// PathsOf are the paths sb's sessions tell an agent whose host workdir and
// home are these: the constants, but under host the dirs themselves.
func PathsOf(sb Sandbox, workdir, home string) Paths {
	if h, ok := sb.(interface{ paths(string, string) Paths }); ok {
		return h.paths(workdir, home)
	}
	return Paths{Work: WorkMount, Home: HomeMount, Prepare: PrepareMount}
}

// hostProxyReady bounds the wait for a proxy child's listening line.
var hostProxyReady = 15 * time.Second

// proxyChild is `pats egress run` as a child of this process, its audit
// (stdout) going to a file: the host and bwrap drivers run their proxy so.
type proxyChild struct {
	cmd   *exec.Cmd
	done  chan struct{} // closed once it has exited
	audit *os.File
	addr  string // what it listens on, from its listening line
	// forwarder is the static pats bwrap binds into a sandbox, beside it.
	forwarder string
}

// startProxyChild runs exe (this pats when "") as the proxy with env over
// pats's own, the machine's proxy included, so it chains through it; the
// policy and CA ride in env, never argv. it waits for the listening line,
// bounded; a proxy that fails to listen is stopped and reaped.
func startProxyChild(ctx context.Context, exe string, env map[string]string, auditPath string) (*proxyChild, error) {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("egress: find pats: %w", err)
		}
	}
	cmd := exec.Command(exe, "egress", "run")
	cmd.Env = append(os.Environ(), sortedEnv(env)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
	pc := &proxyChild{cmd: cmd, done: make(chan struct{})}
	if auditPath != "" {
		f, err := os.Create(auditPath)
		if err != nil {
			return nil, fmt.Errorf("egress: audit log: %w", err)
		}
		pc.audit, cmd.Stdout = f, f
	}
	stderr, err := cmd.StderrPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if pc.audit != nil {
			pc.audit.Close()
		}
		return nil, fmt.Errorf("egress: start proxy: %w", err)
	}
	addr := make(chan string, 1)
	go func() {
		br := bufio.NewReader(stderr)
		last := ""
		for {
			line, err := br.ReadString('\n')
			if a, ok := strings.CutPrefix(strings.TrimSpace(line), "listening "); ok {
				addr <- a
				_, _ = io.Copy(io.Discard, br)
				return
			}
			if t := strings.TrimSpace(line); t != "" {
				last = t
			}
			if err != nil {
				addr <- "!" + last
				return
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(pc.done)
	}()
	fail := func(err error) (*proxyChild, error) {
		pc.stop()
		return nil, err
	}
	select {
	case a := <-addr:
		if strings.HasPrefix(a, "!") {
			return fail(fmt.Errorf("egress: proxy exited before listening: %s", a[1:]))
		}
		pc.addr = a
	case <-time.After(hostProxyReady):
		return fail(fmt.Errorf("egress: proxy not listening after %s", hostProxyReady))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	return pc, nil
}

// stop ends the proxy's group, gracefully first, and reaps it.
func (pc *proxyChild) stop() {
	_ = syscall.Kill(-pc.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-pc.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pc.cmd.Process.Pid, syscall.SIGKILL)
		<-pc.done
	}
	if pc.audit != nil {
		_ = pc.audit.Close()
	}
}
