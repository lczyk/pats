// Package sandbox runs a command in an isolated environment: it takes a Spec
// (argv + workdir + env + mounts + egress policy) and runs it, streaming
// output. drivers: container (docker/podman) now, bwrap later.
//
// the host Workdir is bound at WorkMount and used as the working directory;
// everything else the process needs goes in via Env and Mounts. a non-zero
// exit is a result, not a Go error -- see Sandbox.
//
// egress policy (Spec.Egress) controls the sandbox's network:
//
//	open  -> unrestricted
//	none  -> no network at all
//	proxy -> no direct route out; an internal-only network plus a filtering
//	         proxy sidecar (see src/sandbox/proxy) that decides by the policy
//	         and audits every request as json. hosts named by a url entry get
//	         their tls terminated with a per-run CA so requests are filtered
//	         by full url. the CA key reaches the proxy over env only and
//	         never touches disk; the sandboxed process gets the CA cert and a
//	         merged trust bundle, never the key.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// WorkMount is the in-sandbox path the host Workdir is bound to + used as cwd.
const WorkMount = "/workspace"

// Spec is one isolated execution.
type Spec struct {
	Argv    []string          // command + args run inside the sandbox
	Workdir string            // host dir bound at WorkMount and used as cwd
	Env     map[string]string // environment seen by the command
	Mounts  []Mount           // extra host->container binds (e.g. creds, home)
	Egress  Egress            // outbound network policy (zero value = open)
	Sudo    bool              // the agent may sudo (lxd only)
}

// Egress is the outbound network policy for one run. see the package doc for
// the mode semantics.
type Egress struct {
	None      bool         // no network at all
	Set       bool         // the proxy runs with Policy
	Policy    proxy.Policy // what the proxy enforces
	Image     string       // proxy sidecar image; empty -> the published build of src/sandbox/proxy
	AuditPath string       // where to write the proxy's json audit log; "" discards it
	SelfTest  bool         // prove the sandbox has no route out before running
}

// Mount is an extra host path bound into the sandbox.
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// Sandbox runs a Spec in isolation. Start brings it up idle and returns a
// Session to push files into, run commands in (as root or as the agent) and
// pull files out of; Run is the one-shot form, Start + one agent Exec of
// spec.Argv with spec.Env + Close. a non-zero exit is NOT a Go error -- only a
// failure to run (launch, context cancel, driver error) returns a non-nil err.
type Sandbox interface {
	Start(ctx context.Context, spec Spec) (Session, error)
	Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error)
}

// Session is one started sandbox. Close tears it down and is safe to call
// more than once.
type Session interface {
	// Push copies a host file or directory to path inside the sandbox.
	Push(ctx context.Context, hostPath, path string) error
	// Pull copies path inside the sandbox into hostDir.
	Pull(ctx context.Context, path, hostDir string) error
	// Exec runs cmd to completion, streaming its output. on ctx cancel the
	// sandbox is stopped, since stopping the driver's client does not stop
	// the process inside.
	Exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error)
	// HandOver ends the root part of a session: what root left under paths
	// goes to the agent user (a missing path is skipped), and the agent gets
	// the sudo the spec allows. called once, before the agent runs.
	HandOver(ctx context.Context, paths ...string) error
	// Sync brings the agent's writes to the workdir and the writable mounts
	// back to their host paths; a no-op where they are bound.
	Sync(ctx context.Context) error
	Close() error
}

// Cmd is one process run in a started sandbox.
type Cmd struct {
	Argv []string
	Env  map[string]string // on top of the sandbox's own (the egress env among it)
	Root bool              // as root; otherwise as the agent user
	Dir  string            // working directory; "" is WorkMount
}

// run is Run for any Sandbox: start, exec the agent, close.
func run(ctx context.Context, sb Sandbox, spec Spec, stdout, stderr io.Writer) (int, error) {
	sess, err := sb.Start(ctx, spec)
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	paths := []string{WorkMount}
	for _, m := range spec.Mounts {
		if !m.ReadOnly {
			paths = append(paths, m.Container)
		}
	}
	if err := sess.HandOver(ctx, paths...); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return sess.Exec(ctx, Cmd{Argv: spec.Argv, Env: spec.Env}, stdout, stderr)
}

// ErrLaunch marks a Run error raised before the sandboxed process ever started
// (workdir/mount resolution, egress setup). callers need the distinction: such
// a run produced no output at all, whereas a process that started and was then
// killed -- by the deadline, say -- left files behind worth collecting. setup
// runs under the caller's context, so "the deadline fired" alone can't tell the
// two apart.
var ErrLaunch = errors.New("sandbox never launched")

// Options picks and configures a driver.
type Options struct {
	Driver string // docker | podman | lxd
	Image  string // the image to start: a container image, or an lxd image alias
	Limits Limits
	// lxd only.
	Remote   string            // an lxc remote; "" -> the cli's current one
	VM       bool              // a virtual machine rather than a system container
	Profiles []string          // in the pats project
	Config   map[string]string // instance config, passed through
}

// Limits caps a sandbox's resources; zero values leave the driver's default.
type Limits struct {
	CPU    int
	Memory string // e.g. 8GiB
	Disk   string // lxd only
}

// New builds a Sandbox for o.Driver. every driver needs an image.
func New(o Options) (Sandbox, error) {
	switch o.Driver {
	case "docker", "podman":
		if o.Image == "" {
			return nil, fmt.Errorf("sandbox driver %q needs an image", o.Driver)
		}
		return &container{driver: o.Driver, bin: o.Driver, image: o.Image, limits: o.Limits}, nil
	case "lxd":
		if o.Image == "" {
			return nil, errors.New("sandbox driver lxd needs an image")
		}
		return newLXD(o), nil
	case "bwrap":
		return nil, errors.New("sandbox driver bwrap not implemented yet")
	case "":
		return nil, errors.New("sandbox: empty driver")
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", o.Driver)
	}
}

// container drives docker / podman (cli-compatible). driver is which of the
// two; bin is the executable, the same name outside tests.
type container struct {
	driver string
	bin    string
	image  string
	limits Limits
	// exec/execOut run the cli, swappable in tests (nil -> real exec). they
	// cover the setup/teardown plumbing (egress etc.); the main agent `run`
	// streams via exec.CommandContext directly and stays e2e-tested.
	exec    func(ctx context.Context, bin string, args ...string) (string, error)
	execOut func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

func (c *container) docker(ctx context.Context, args ...string) (string, error) {
	if c.exec != nil {
		return c.exec(ctx, c.bin, args...)
	}
	return docker(ctx, c.bin, args...)
}

func (c *container) dockerOut(ctx context.Context, args ...string) ([]byte, error) {
	if c.execOut != nil {
		return c.execOut(ctx, c.bin, args...)
	}
	return dockerOut(ctx, c.bin, args...)
}

func (c *container) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	return run(ctx, c, spec, stdout, stderr)
}

// Start brings the container up idle (`sleep infinity` under --init) as root,
// so a prepare step can run as root and the agent as the host uid, each by
// exec. the workdir and mounts are bound, so the agent's files are on the
// host as it writes them.
func (c *container) Start(ctx context.Context, spec Spec) (Session, error) {
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workdir: %w", ErrLaunch, err)
	}
	// egress policy: none -> no network; proxy -> sidecar on an internal net.
	netArgs, teardown, err := c.setupEgress(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if teardown == nil {
		teardown = func() {}
	}

	args := []string{
		"run", "-d", "--rm",
		"--init", // reap zombies if the agent spawns children
		// an agent run as root (pats itself as root) could otherwise craft raw
		// frames past its network's routing, or gain privileges via setuid.
		"--cap-drop", "NET_RAW", "--security-opt", "no-new-privileges",
		"-w", WorkMount,
		"-v", abs + ":" + WorkMount,
	}
	if name := agentName(abs); name != "" {
		args = append(args, "--name", name, "--label", OwnerLabel())
	}
	args = append(args, netArgs...)
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			teardown()
			return nil, fmt.Errorf("%w: resolve mount %s: %w", ErrLaunch, m.Host, err)
		}
		v := mh + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	if c.limits.CPU > 0 {
		args = append(args, "--cpus", fmt.Sprint(c.limits.CPU))
	}
	if c.limits.Memory != "" {
		args = append(args, "--memory", c.limits.Memory)
	}
	// the cid file names the container even when the cli is killed after the
	// daemon made it, so a failed or cancelled start removes it.
	cidf, err := os.CreateTemp("", "pats-cid-")
	if err != nil {
		teardown()
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	cid := cidf.Name()
	cidf.Close()
	os.Remove(cid) // the cli refuses an existing file
	defer os.Remove(cid)
	args = append(args, "--cidfile", cid)
	args = append(args, "--entrypoint", "sleep", c.image, "infinity")
	out, err := c.dockerOut(ctx, args...)
	if err != nil {
		if b, rerr := os.ReadFile(cid); rerr == nil && len(b) > 0 {
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = c.docker(rctx, "rm", "-f", strings.TrimSpace(string(b)))
			cancel()
		}
		teardown()
		return nil, fmt.Errorf("%w: %s run failed (is the daemon up? `docker info`; on apple silicon an amd64 image may need --platform): %w", ErrLaunch, c.bin, err)
	}
	id := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(id, '\n'); i >= 0 {
		id = id[i+1:] // a pull's progress may precede the id
	}
	return &containerSession{c: c, id: id, teardown: teardown}, nil
}

// containerSession is a started container, addressed by id.
type containerSession struct {
	c        *container
	id       string
	teardown func() // the egress sidecar and network
	once     sync.Once
	rootRan  bool // a root exec happened, so HandOver has work to do
}

func (s *containerSession) Push(ctx context.Context, hostPath, path string) error {
	if out, err := s.c.docker(ctx, "cp", hostPath, s.id+":"+path); err != nil {
		return fmt.Errorf("%s cp %s: %w (%s)", s.c.bin, hostPath, err, out)
	}
	return nil
}

func (s *containerSession) Pull(ctx context.Context, path, hostDir string) error {
	if out, err := s.c.docker(ctx, "cp", s.id+":"+path, hostDir); err != nil {
		return fmt.Errorf("%s cp %s: %w (%s)", s.c.bin, path, err, out)
	}
	return nil
}

func (s *containerSession) Exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if cmd.Root {
		user = "0:0"
		s.rootRan = true
	}
	dir := cmd.Dir
	if dir == "" {
		dir = WorkMount
	}
	// the agent runs as the host uid so the files it writes into the bound
	// workdir are host-owned -- else collect + cleanup hit root-owned files
	// on linux. harness images must tolerate an arbitrary uid (writable
	// HOME/tmp).
	args := []string{"exec", "-u", user, "-w", dir}
	for _, kv := range sortedEnv(cmd.Env) {
		args = append(args, "-e", kv)
	}
	args = append(args, s.id)
	args = append(args, cmd.Argv...)

	x := exec.CommandContext(ctx, s.c.bin, args...)
	x.Stdout = stdout
	x.Stderr = stderr
	// on ctx cancel (ctrl+C, the deadline) kill the container: ending the
	// exec client leaves the process inside running.
	x.Cancel = func() error {
		_, _ = s.c.docker(context.Background(), "kill", s.id)
		return x.Process.Signal(syscall.SIGTERM)
	}
	x.WaitDelay = 10 * time.Second

	err := x.Run()
	if err == nil {
		return 0, nil
	}
	if e, ok := asExitError(err); ok {
		return e.ExitCode(), nil // ran; non-zero exit is the command's
	}
	return -1, fmt.Errorf("%s exec failed: %w", s.c.bin, err)
}

func (s *containerSession) Sync(ctx context.Context) error { return nil }

func (s *containerSession) HandOver(ctx context.Context, paths ...string) error {
	if !s.rootRan {
		return nil // nothing root could have left
	}
	script := fmt.Sprintf(`for p; do [ -e "$p" ] || continue; chown -R %d:%d "$p" || exit 1; done`, os.Getuid(), os.Getgid())
	var out strings.Builder
	code, err := s.Exec(ctx, Cmd{Argv: append([]string{"/bin/sh", "-c", script, "handover"}, paths...), Root: true}, &out, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("hand over %s: %w", strings.Join(paths, " "), err)
	}
	return nil
}

func (s *containerSession) Close() error {
	s.once.Do(func() {
		_, _ = s.c.docker(context.Background(), "rm", "-f", s.id)
		s.teardown()
	})
	return nil
}

// asExitError extracts a *exec.ExitError from err. cmd.Cancel/WaitDelay above
// can wrap the exit error inside another error (e.g. on ctx cancel racing the
// process exit), so this must use errors.As -- a bare `err.(*exec.ExitError)`
// type assertion panics on exactly that wrapped case.
func asExitError(err error) (*exec.ExitError, bool) {
	var e *exec.ExitError
	ok := errors.As(err, &e)
	return e, ok
}

// sortedEnv renders the env map as sorted KEY=VALUE entries (stable argv).
func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
