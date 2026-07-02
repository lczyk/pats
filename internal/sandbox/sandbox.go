// Package sandbox runs a command in an isolated environment. it knows nothing
// about agents or scoring -- it takes an ExecSpec (argv + cwd + env) and runs
// it, streaming output. drivers: container (docker/podman) now, bwrap later.
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
)

// WorkMount is the in-sandbox path the host Workdir is bound to + used as cwd.
const WorkMount = "/workspace"

// Spec is one isolated execution.
type Spec struct {
	Argv    []string          // command + args run inside the sandbox
	Workdir string            // host dir bound at WorkMount and used as cwd
	Env     map[string]string // environment (PATS_* + passthrough)
	Mounts  []Mount           // extra host->container binds (e.g. creds, home)
	Egress  Egress            // outbound network policy (zero value = open)
}

// Egress is the resolved network policy for one run (from config.Sandbox.Egress).
type Egress struct {
	Mode      string // "" | open | none | proxy | mitm-proxy
	Default   string // proxy: deny | allow
	Allow     []string
	Deny      []string
	DenyURLs  []string // mitm-proxy: host-anchored url patterns to block (deny wins)
	AllowURLs []string // mitm-proxy: a host with allow rules only passes matching urls
	Image     string   // proxy image
	AuditPath string   // where to write the proxy's json audit log (proxy mode)
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

// New builds a Sandbox for the given driver. container drivers need an image.
func New(driver, image string) (Sandbox, error) {
	switch driver {
	case "docker", "podman":
		if image == "" {
			return nil, fmt.Errorf("sandbox driver %q needs an image", driver)
		}
		return &container{bin: driver, image: image}, nil
	case "bwrap":
		return nil, errors.New("sandbox driver bwrap not implemented yet")
	case "":
		return nil, errors.New("sandbox: empty driver")
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", driver)
	}
}

// container drives docker / podman (cli-compatible).
type container struct {
	bin   string
	image string
}

func (c *container) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return -1, fmt.Errorf("resolve workdir: %w", err)
	}
	// egress policy: none -> no network; proxy -> sidecar on an internal net.
	netArgs, teardown, err := c.setupEgress(ctx, spec)
	if err != nil {
		return -1, err
	}
	if teardown != nil {
		defer teardown()
	}

	args := []string{
		"run", "--rm",
		"--init", // reap zombies if the agent spawns children
		// run as the host user so files the agent writes into the bind-mounted
		// workdir are host-owned -- else collect + cleanup hit root-owned files
		// on linux. harness images must tolerate an arbitrary uid (writable
		// HOME/tmp); that's handled when harness presets land.
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-w", WorkMount,
		"-v", abs + ":" + WorkMount,
	}
	args = append(args, netArgs...)
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			return -1, fmt.Errorf("resolve mount %s: %w", m.Host, err)
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

	switch err := cmd.Run(); {
	case err == nil:
		return 0, nil
	case isExit(err):
		return err.(*exec.ExitError).ExitCode(), nil // ran; non-zero exit is the agent's
	default:
		return -1, fmt.Errorf("%s run failed (is the daemon up? `docker info`; on apple silicon an amd64 image may need --platform): %w", c.bin, err)
	}
}

func isExit(err error) bool {
	var e *exec.ExitError
	return errors.As(err, &e)
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
