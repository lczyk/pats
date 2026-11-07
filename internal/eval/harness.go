package eval

import (
	"context"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"time"

	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// harness is one agent invocation wired into a sandbox: the box, the spec
// (argv, env, mounts, egress) and the writable HOME its creds were copied
// into. the HOME lives until cleanup, so a caller can seed it before run and
// read it back after.
type harness struct {
	box     sandbox.Sandbox
	spec    sandbox.Spec
	homeDir string // host side of homeMount
	cleanup func()
}

// promptName is the prompt file's name inside the harness HOME. it lives there,
// not in the workdir, so the agent's cwd holds only what stage put in it: a
// stray prompt.txt next to the fixture gets noticed and commented on.
const promptName = "prompt.txt"

// newHarness resolves agent a into sandbox sb, with te (the task's egress
// additions; zero for preflight) folded into the policy. the caller writes the prompt to
// promptPath() before run; runID "" (preflight) leaves PATS_RUN_ID unset and
// turns on the egress self-test; auditPath "" turns the egress audit log off;
// label prefixes the no-creds warning.
func newHarness(opts Options, a config.Agent, sb config.Sandbox, te config.TaskEgress, workDir, runID, auditPath, label string) (*harness, error) {
	box, err := newSandbox(sandbox.Options{
		Driver: sb.Driver, Image: sb.Image, Remote: sb.Remote, VM: sb.VM, Profiles: sb.Profiles, Config: sb.Config,
		Limits: sandbox.Limits{CPU: sb.Limits.CPU, Memory: sb.Limits.Memory, Disk: sb.Limits.Disk},
	})
	if err != nil {
		return nil, err
	}
	env := maps.Clone(sb.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, agent.Env(a, homeMount+"/"+promptName, sandbox.WorkMount, runID))
	cenv, hasToken := agent.CredEnv(a.Harness)
	maps.Copy(env, cenv) // forward host creds to any task-running agent

	hs, err := harnessHome(opts, label, a.Harness, env, hasToken)
	if err != nil {
		return nil, err
	}
	spec, err := agent.Spec(a, workDir, env)
	if err != nil {
		hs.cleanup()
		return nil, err
	}
	spec.Mounts = hs.mounts
	spec.Egress = egressFor(sb, te, a.Harness, auditPath, runID == "")
	spec.Sudo = sb.Sudo
	return &harness{box: box, spec: spec, homeDir: hs.dir, cleanup: hs.cleanup}, nil
}

// promptPath is the host path the prompt must be written to before run.
func (h *harness) promptPath() string { return filepath.Join(h.homeDir, promptName) }

// harnessRun is the outcome of one harness.run.
type harnessRun struct {
	code     int
	err      error // launch / driver failure; nil on a plain non-zero exit
	timedOut bool
	dur      float64 // seconds
}

// run executes the harness in a sandbox of its own (start, agent, close),
// bounded by timeout (0 = none) so a stuck agent gets killed rather than
// hanging the caller. ctx cancel tears down the container (same path as
// ctrl+C).
func (h *harness) run(ctx context.Context, stdout, stderr io.Writer, timeout time.Duration) harnessRun {
	return bounded(ctx, timeout, func(rctx context.Context) (int, error) {
		return h.box.Run(rctx, h.spec, stdout, stderr)
	})
}

// exec runs the agent in an already started session, bounded the same way.
func (h *harness) exec(ctx context.Context, sess sandbox.Session, stdout, stderr io.Writer, timeout time.Duration) harnessRun {
	return bounded(ctx, timeout, func(rctx context.Context) (int, error) {
		return sess.Exec(rctx, sandbox.Cmd{Argv: h.spec.Argv, Env: h.spec.Env}, stdout, stderr)
	})
}

func bounded(ctx context.Context, timeout time.Duration, f func(context.Context) (int, error)) harnessRun {
	rctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	t0 := time.Now()
	code, err := f(rctx)
	dur := time.Since(t0).Seconds()

	// decide timeout from rctx.Err(), not err: on deadline the sandbox kills
	// the container, which exits non-zero (137) -- the driver maps any
	// *exec.ExitError to (code, nil), so err is nil on a timed-out run too.
	// ctrl+C (context.Canceled on the parent ctx) is not a timeout. the one
	// exception is a launch failure: sandbox setup (egress in particular) can
	// run under the same deadline, so a slow setup can fail *and* leave rctx
	// expired -- that's an error with nothing to collect, not a timed-out
	// agent, and the real cause belongs in the metadata.
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded) && !errors.Is(err, sandbox.ErrLaunch)
	return harnessRun{code: code, err: err, timedOut: timedOut, dur: dur}
}
