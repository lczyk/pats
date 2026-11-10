package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// run runs one command of this machine to its end, in a process group
	// of its own that goes with ctx. the driver's seam, as a cli runner is
	// the others': a test answers for the machine.
	run func(ctx context.Context, cmd *exec.Cmd) error
}

func newHost(p *process, o Options) *host {
	// the agent runs in a workdir of its own: a relative SSL_CERT_FILE
	// would name nothing there.
	if o.TrustFile != "" {
		if abs, err := filepath.Abs(o.TrustFile); err == nil {
			o.TrustFile = abs
		}
	}
	return &host{p: p, o: o, run: RunGroup}
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
	rctx, cancel := context.WithTimeout(ctx, h.p.t.stamp)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", stampScript)
	cmd.Env = hostEnv(nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := h.run(rctx, cmd); err != nil {
		return nil, fmt.Errorf("harness versions on this machine: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseStamps(out.String())
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
	pc, err := startProxyChild(ctx, s.b.h.p, s.b.h.pats, mergeEnv(penv, map[string]string{"PATS_EGRESS_LISTEN": "127.0.0.1:0"}), eg.AuditPath)
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
	err = s.b.h.run(xctx, cmd)
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
