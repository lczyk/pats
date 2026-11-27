package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// preflightPrompt is a throwaway task that only succeeds if the harness can
// authenticate and reach its model -- the cheapest faithful credential check.
const preflightPrompt = "Reply with the single word: OK. Do nothing else."

// preflightTimeout bounds a single credential check: the agent's reply. its
// sandbox has setupTimeout to come up, as a pair's does -- on a cold lxd
// server the first instance from a fresh image takes minutes.
const preflightTimeout = 90 * time.Second

// Preflight runs each distinct test-matrix agent once with a trivial prompt,
// through its real sandbox + egress + creds path. catches expired/missing
// creds up front instead of after a per-pair clone + container spin. a step
// sandbox that fails its check is the error; every agent is checked whatever
// an earlier one found, and its failures come back in selection order
// (docs/spec/preflight-per-agent.md). err is also ctx's, once it ends.
func Preflight(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair, judged ...config.Scorer) ([]agentFailure, error) {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })

	// each step sandbox the tasks name starts once, with its checks, and
	// runs a step: a box that cannot see the tree fails here, not mid-run,
	// and before the credential checks, since it needs none.
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.Name })
	var placed []config.Task
	for _, p := range pairs {
		placed = append(placed, tasks[p.Task])
	}
	for _, sc := range judged {
		placed = append(placed, sc.Task())
	}
	done := map[string]bool{}
	for _, t := range placed {
		sp, err := stepPlaceOf(cfg, sandboxes, t.StepSandbox, t.StepEnv)
		if err != nil {
			return nil, err
		}
		if sp == nil || done[sp.sb.Name] {
			continue
		}
		done[sp.sb.Name] = true
		t0 := time.Now()
		if err := preflightSteps(ctx, opts, sp); err != nil {
			return nil, fmt.Errorf("step sandbox %q failed preflight: %w", sp.sb.Name, err)
		}
		logw{opts.Out, opts.Color, opts.Verbose}.detail("preflight: step sandbox %s ok (%.1fs)", sp.sb.Name, time.Since(t0).Seconds())
	}
	var failed []agentFailure
	seen := map[string]bool{}
	for _, p := range pairs {
		if seen[p.Agent] {
			continue
		}
		seen[p.Agent] = true
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
		t0 := time.Now()
		if f := preflightAgent(ctx, cfg, opts, agents[p.Agent], sandboxes); f != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			f.agent = p.Agent
			logw{opts.Out, opts.Color, opts.Verbose}.error("%v", f)
			failed = append(failed, *f)
			continue
		}
		logw{opts.Out, opts.Color, opts.Verbose}.detail("preflight: %s ok (%.1fs)", p.Agent, time.Since(t0).Seconds())
	}
	return failed, nil
}

// failKind is whose a preflight failure is: the agent's (its harness, its
// key), the place's (its sandbox), or the invocation's (this machine's or
// this config's, which every agent would meet).
type failKind int

const (
	failAgent failKind = iota
	failPlace
	failInvocation
)

func (k failKind) String() string {
	switch k {
	case failPlace:
		return "place"
	case failInvocation:
		return "invocation"
	}
	return "harness"
}

// agentFailure is an agent's failed preflight.
type agentFailure struct {
	agent string
	kind  failKind
	err   error
}

func (f agentFailure) Error() string {
	return fmt.Sprintf("agent %q failed credential preflight (%s): %v", f.agent, f.kind, f.err)
}

func (f agentFailure) Unwrap() error { return f.err }

// failOf is the first failure of the worst kind one check met: the
// invocation's, then the place's, then the agent's.
func failOf(fs ...*agentFailure) *agentFailure {
	var worst *agentFailure
	for _, f := range fs {
		if f != nil && (worst == nil || f.kind > worst.kind) {
			worst = f
		}
	}
	return worst
}

func failed(kind failKind, err error) *agentFailure { return &agentFailure{kind: kind, err: err} }

// preflightSteps starts a step sandbox on the tree alone and runs `true`
// in it as a step.
func preflightSteps(ctx context.Context, opts Options, sp *stepPlace) error {
	mounts, err := stepBindsFor(opts.ConfigDir, nil, nil)
	if err != nil {
		return err
	}
	box, err := openStepBox(ctx, sp, mounts, "preflight-egress.log", configProbe(opts.ConfigDir))
	if err != nil {
		return err
	}
	defer box.close("")
	return box.run(ctx, sp, step{script: "true", dir: opts.ConfigDir}, opts.ConfigDir, nil, io.Discard, io.Discard)
}

func preflightAgent(ctx context.Context, cfg *config.Config, opts Options, a config.Agent, sandboxes map[string]config.Sandbox) *agentFailure {
	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return failed(failInvocation, err)
	}
	sb := sandboxes[sbID]
	if sandbox.TraitsOf(sb.Driver).HostCLIs {
		// no image carries the cli: the machine's PATH must.
		if hd := cfg.HarnessOf(a); hd != nil && hd.CLI != "" {
			if _, err := sandbox.HostLookPath(hd.CLI); err != nil {
				return failed(failAgent, fmt.Errorf("its harness cli %s is not on this machine's PATH; install it", hd.CLI))
			}
		}
	}
	lw := logw{opts.Out, opts.Color, opts.Verbose}
	if sandbox.TraitsOf(sb.Driver).Unboxed && sb.Egress.Set {
		// no fail-closed self-test to run: nothing stops what goes around
		// the proxy.
		lw.info("preflight: %s: egress is advisory under the host driver", a.Name)
	}

	workDir, err := sandbox.MkTemp("pats-preflight-")
	if err != nil {
		return failed(failInvocation, err)
	}
	defer sandbox.RemoveTemp(workDir)

	// same egress policy as real runs. no audit, but under a key proxy: its
	// own, outside the workdir the sandbox sees, to show a key went out.
	auditPath := ""
	if sb.Egress.KeyProxy {
		dir, err := sandbox.MkTemp("pats-preflight-audit-")
		if err != nil {
			return failed(failInvocation, err)
		}
		defer sandbox.RemoveTemp(dir)
		auditPath = filepath.Join(dir, "egress.log")
	}
	hd := cfg.HarnessOf(a)
	h, err := newHarness(opts, a, hd, sb, config.TaskEgress{}, workDir, "", auditPath, a.Name+" x preflight")
	if err != nil {
		return failed(failInvocation, err)
	}
	defer h.cleanup()
	// a harness that cannot answer a prompt cheaply (a script, a baseline)
	// sees this and exits 0.
	h.spec.Env["PATS_PREFLIGHT"] = "1"
	if err := os.WriteFile(h.promptPath(), []byte(preflightPrompt), 0o644); err != nil {
		return failed(failInvocation, err)
	}

	lw.info("preflight: %s: starting sandbox", a.Name)
	h.spec.Progress = func(m string) { lw.info("preflight: %s: %s", a.Name, m) }
	t0 := time.Now()
	sctx, scancel := context.WithTimeout(ctx, setupTimeout)
	sess, err := h.box.Start(sctx, h.spec)
	if err == nil {
		if herr := sess.HandOver(sctx, h.paths.Work, h.paths.Home); herr != nil {
			scancel()
			closed := closeFailure(sess)
			var audit *agentFailure
			if auditPath != "" {
				// no request went out before the hand-over, so no key can have been misused.
				audit, _ = keyProxyChecked(auditPath, false)
			}
			return failOf(audit, closed, failed(failPlace, fmt.Errorf("hand-over: %w", herr)))
		}
	}
	scancel()
	if err != nil {
		return failed(failPlace, fmt.Errorf("start: %w", err))
	}
	lw.info("preflight: %s: sandbox up (%.1fs), running the harness", a.Name, time.Since(t0).Seconds())
	// stdout and stderr land in one buffer, for the error's tail. stdout is
	// also read as a pair's is, for what it says of cost, so the two are no
	// longer one writer and the buffer takes a lock.
	var buf lockedBuffer
	stat := &pairStat{price: opts.prices[a.Name]}
	tap := &statTap{stat: stat, scan: statScanner(hd)}
	r := h.exec(ctx, sess, io.MultiWriter(&buf, tap), &buf, preflightTimeout)
	harness := harnessFailure(r, stat, buf.String())
	// the session closes once, on every path, before the audit is read: the
	// proxy's log is drained only then.
	closed := closeFailure(sess)
	var audit, key *agentFailure
	if auditPath != "" {
		audit, key = keyProxyChecked(auditPath, len(h.spec.Egress.Policy.Keys) > 0)
	}
	if f := failOf(audit, closed, harness, key); f != nil {
		return f
	}
	// a cap is checked against what a harness reports: one that reported
	// none of a kind here leaves a cap of it nothing to check. preflight
	// asks for no tool call, so for tools only a harness with no parse
	// script is known to report none. it always takes a turn, so a harness
	// that counted none here reports none.
	if (a.Budget.Cost > 0 || opts.Budget.Cost > 0) && !stat.costSeen.Load() {
		lw.warn("agent %s reports no cost: its budget has nothing to check", a.Name)
	}
	if (a.Budget.Tokens > 0 || opts.Budget.Tokens > 0) && !stat.tokensSeen.Load() {
		lw.warn("agent %s reports no tokens: its tokens budget has nothing to check", a.Name)
	}
	if (a.Budget.Tools > 0 || opts.Budget.Tools > 0) && hd.Parser() == nil {
		lw.warn("agent %s reports no tool calls: its tools budget has nothing to check", a.Name)
	}
	if (a.Budget.Turns > 0 || opts.Budget.Turns > 0) && stat.liveTurns.Load() == 0 {
		lw.warn("agent %s reports no turns: its turns budget has nothing to check", a.Name)
	}
	return nil
}

// harnessFailure is what the harness's run says of its credentials, nil
// when it answered.
func harnessFailure(r harnessRun, stat *pairStat, out string) *agentFailure {
	if r.err != nil {
		return failed(failAgent, fmt.Errorf("%w\n%s", r.err, tail(out, 600)))
	}
	// what the harness said of its error, or failing that the end of its output.
	said, _ := stat.failure.Load().(string)
	if said == "" {
		said = tail(out, 600)
	}
	if r.code != 0 {
		return failed(failAgent, fmt.Errorf("harness exited %d (likely bad/expired creds)\n%s", r.code, said))
	}
	// some harnesses exit 0 on a reply that failed, and say so in their stream.
	if stat.failed.Load() {
		return failed(failAgent, fmt.Errorf("harness reported an error (likely bad/expired creds)\n%s", said))
	}
	return nil
}

// closeFailure closes a preflight's session: a close that fails is the
// place's.
func closeFailure(sess sandbox.Session) *agentFailure {
	if err := sess.Close(); err != nil {
		return failed(failPlace, fmt.Errorf("close: %w", err))
	}
	return nil
}

// lockedBuffer is a buffer two streams write to at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tail returns the last n bytes of s, prefixed when truncated.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}

// keyProxyChecked reads a preflight's audit once its session is closed (the
// proxy's log drained). an audit missing or unreadable is the invocation's;
// a misuse is the agent's, as is, when a key was held, no key going out at
// all, since a harness can exit 0 without having authenticated.
func keyProxyChecked(auditPath string, held bool) (audit, key *agentFailure) {
	ea, err := readEgressAuditErr(auditPath)
	if err != nil {
		return failed(failInvocation, fmt.Errorf("key proxy: the preflight's audit: %w", err)), nil
	}
	if len(ea.KeyMisuse) > 0 {
		return nil, failed(failAgent, fmt.Errorf("key proxy: a placeholder was refused off its binding at %s", strings.Join(ea.KeyMisuse, ", ")))
	}
	if held && ea.Substituted == 0 {
		return nil, failed(failAgent, errors.New("key proxy: the preflight sent no request the proxy put a key in; the harness never authenticated through it"))
	}
	return nil, nil
}
