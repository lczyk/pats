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

// Sandbox runs a Spec in isolation, streaming stdout/stderr to the writers and
// returning the command's exit code. a non-zero exit is NOT a Go error -- only
// a failure to run (launch, context cancel, driver error) returns a non-nil err.
type Sandbox interface {
	Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error)
}

// ErrLaunch marks a Run error raised before the sandboxed process ever started
// (workdir/mount resolution, egress setup). callers need the distinction: such
// a run produced no output at all, whereas a process that started and was then
// killed -- by the deadline, say -- left files behind worth collecting. setup
// runs under the caller's context, so "the deadline fired" alone can't tell the
// two apart.
var ErrLaunch = errors.New("sandbox never launched")

// New builds a Sandbox for the given driver. container drivers need an image.
func New(driver, image string) (Sandbox, error) {
	switch driver {
	case "docker", "podman":
		if image == "" {
			return nil, fmt.Errorf("sandbox driver %q needs an image", driver)
		}
		return &container{driver: driver, bin: driver, image: image}, nil
	case "bwrap":
		return nil, errors.New("sandbox driver bwrap not implemented yet")
	case "":
		return nil, errors.New("sandbox: empty driver")
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", driver)
	}
}

// container drives docker / podman (cli-compatible). driver is which of the
// two; bin is the executable, the same name outside tests.
type container struct {
	driver string
	bin    string
	image  string
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
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return -1, fmt.Errorf("%w: resolve workdir: %w", ErrLaunch, err)
	}
	// egress policy: none -> no network; proxy -> sidecar on an internal net.
	netArgs, teardown, err := c.setupEgress(ctx, spec)
	if err != nil {
		return -1, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if teardown != nil {
		defer teardown()
	}

	args := []string{
		"run", "--rm",
		"--init", // reap zombies if the agent spawns children
		// an agent run as root (pats itself as root) could otherwise craft raw
		// frames past its network's routing, or gain privileges via setuid.
		"--cap-drop", "NET_RAW", "--security-opt", "no-new-privileges",
		// run as the host user so files the agent writes into the bind-mounted
		// workdir are host-owned -- else collect + cleanup hit root-owned files
		// on linux. harness images must tolerate an arbitrary uid (writable
		// HOME/tmp); that's handled when harness presets land.
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-w", WorkMount,
		"-v", abs + ":" + WorkMount,
	}
	if name := agentName(abs); name != "" {
		args = append(args, "--name", name)
	}
	args = append(args, netArgs...)
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			return -1, fmt.Errorf("%w: resolve mount %s: %w", ErrLaunch, m.Host, err)
		}
		v := mh + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "-e", kv)
	}
	args = append(args, c.image)
	args = append(args, spec.Argv...)

	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// on ctx cancel (ctrl+C), SIGTERM the docker client so it stops the
	// container (--rm then removes it) instead of orphaning it on SIGKILL.
	// WaitDelay forces a hard kill if it doesn't exit in time.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	if e, ok := asExitError(err); ok {
		return e.ExitCode(), nil // ran; non-zero exit is the agent's
	}
	return -1, fmt.Errorf("%s run failed (is the daemon up? `docker info`; on apple silicon an amd64 image may need --platform): %w", c.bin, err)
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
