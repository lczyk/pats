package eval

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// preflightPrompt is a throwaway task that only succeeds if the harness can
// authenticate and reach its model -- the cheapest faithful credential check.
const preflightPrompt = "Reply with the single word: OK. Do nothing else."

// preflightTimeout bounds a single credential check.
const preflightTimeout = 90 * time.Second

// Preflight runs each distinct test-matrix agent once with a trivial prompt,
// through its real sandbox + egress + creds path, and fails if any can't
// authenticate. catches expired/missing creds up front instead of after a
// per-pair clone + container spin. returns the first failing agent's error.
func Preflight(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair) error {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.ID })

	seen := map[string]bool{}
	for _, p := range pairs {
		if seen[p.Agent] {
			continue
		}
		seen[p.Agent] = true
		if err := preflightAgent(ctx, cfg, opts, agents[p.Agent], sandboxes); err != nil {
			return fmt.Errorf("agent %q failed credential preflight: %w", p.Agent, err)
		}
		logw{opts.Out, opts.Color}.info("preflight: %s ok", p.Agent)
	}
	return nil
}

func preflightAgent(ctx context.Context, cfg *config.Config, opts Options, a config.Agent, sandboxes map[string]config.Sandbox) error {
	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return err
	}
	sb := sandboxes[sbID]

	workDir, err := sandbox.MkTemp("pats-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "prompt.txt"), []byte(preflightPrompt), 0o644); err != nil {
		return err
	}

	// same egress policy as real runs (no audit).
	h, err := newHarness(opts, a, sb, workDir, "prompt.txt", "", a.ID+" x preflight")
	if err != nil {
		return err
	}
	defer h.cleanup()

	var buf bytes.Buffer
	r := h.run(ctx, &buf, &buf, preflightTimeout)
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
