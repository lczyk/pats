package eval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lczyk/pats/internal/agent"
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
// through its real sandbox + egress + creds path, and fails if any can't
// authenticate. catches expired/missing creds up front instead of after a
// per-pair clone + container spin. returns the first failing agent's error.
func Preflight(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair, judged ...config.Scorer) error {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })

	seen := map[string]bool{}
	for _, p := range pairs {
		if seen[p.Agent] {
			continue
		}
		seen[p.Agent] = true
		if err := preflightAgent(ctx, cfg, opts, agents[p.Agent], sandboxes); err != nil {
			return fmt.Errorf("agent %q failed credential preflight: %w", p.Agent, err)
		}
		logw{opts.Out, opts.Color, opts.Verbose}.detail("preflight: %s ok", p.Agent)
	}
	// each step sandbox the tasks name starts once, with its checks, and
	// runs a step: a box that cannot see the tree fails here, not mid-run.
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
			return err
		}
		if sp == nil || done[sp.sb.Name] {
			continue
		}
		done[sp.sb.Name] = true
		if err := preflightSteps(ctx, opts, sp); err != nil {
			return fmt.Errorf("step sandbox %q failed preflight: %w", sp.sb.Name, err)
		}
		logw{opts.Out, opts.Color, opts.Verbose}.detail("preflight: step sandbox %s ok", sp.sb.Name)
	}
	return nil
}

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

func preflightAgent(ctx context.Context, cfg *config.Config, opts Options, a config.Agent, sandboxes map[string]config.Sandbox) error {
	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return err
	}
	sb := sandboxes[sbID]
	if sandbox.TraitsOf(sb.Driver).HostCLIs {
		// no image carries the cli: the machine's PATH must.
		if cli, ok := agent.HarnessCLIs[a.Harness]; ok {
			if _, err := sandbox.HostLookPath(cli); err != nil {
				return fmt.Errorf("its harness cli %s is not on this machine's PATH; install it", cli)
			}
		}
	}
	if sandbox.TraitsOf(sb.Driver).Unboxed && sb.Egress.Set {
		// no fail-closed self-test to run: nothing stops what goes around
		// the proxy.
		logw{opts.Out, opts.Color, opts.Verbose}.info("preflight: %s: egress is advisory under the host driver", a.Name)
	}

	workDir, err := sandbox.MkTemp("pats-preflight-")
	if err != nil {
		return err
	}
	defer sandbox.RemoveTemp(workDir)

	// same egress policy as real runs (no audit).
	h, err := newHarness(opts, a, sb, config.TaskEgress{}, workDir, "", "", a.Name+" x preflight")
	if err != nil {
		return err
	}
	defer h.cleanup()
	if err := os.WriteFile(h.promptPath(), []byte(preflightPrompt), 0o644); err != nil {
		return err
	}

	sctx, scancel := context.WithTimeout(ctx, setupTimeout)
	sess, err := h.box.Start(sctx, h.spec)
	if err == nil {
		defer sess.Close()
		err = sess.HandOver(sctx, h.paths.Work, h.paths.Home)
	}
	scancel()
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	var buf bytes.Buffer
	r := h.exec(ctx, sess, &buf, &buf, preflightTimeout)
	if r.err != nil {
		return fmt.Errorf("%w\n%s", r.err, tail(buf.String(), 600))
	}
	if r.code != 0 {
		return fmt.Errorf("harness exited %d (likely bad/expired creds)\n%s", r.code, tail(buf.String(), 600))
	}
	return nil
}

// tail returns the last n bytes of s, prefixed when truncated.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}
