package eval

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// runAgents runs three agents a1..a3 on one task each with its own fake,
// and returns the run dir, the error and the log.
func runAgents(t *testing.T, fakes map[string]*fakeSandbox, opts Options) (string, error, string) {
	t.Helper()
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fakes[s.Image], nil }
	t.Cleanup(func() { newSandbox = old })
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	src := "boxes: {kind: sandbox, items: {"
	for _, id := range []string{"a1", "a2", "a3"} {
		src += id + ": {image: " + id + "}, "
	}
	src += "}}\nagents: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {"
	for _, id := range []string{"a1", "a2", "a3"} {
		src += id + ": {sandbox: " + id + "}, "
	}
	src += "}}\ntxx: {kind: task, items: {txx: {prompt-file: prompt.txt}}}\nsux: {kind: suite, items: {sux: {agents: agents, tasks: txx}}}\n"
	cfg := parseCfg(t, src)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	opts.ConfigDir, opts.Now, opts.Out = dir, time.Now(), &out
	runDir, err := Run(cfg, opts)
	return runDir, err, out.String()
}

func threeFakes() map[string]*fakeSandbox {
	return map[string]*fakeSandbox{"a1": {}, "a2": {}, "a3": {}}
}

// a failed agent's pairs are not started; the others run, and the run
// ends with ErrPreflight and a line naming it.
func TestPreflightDropsAgent(t *testing.T) {
	for kind, fail := range map[string]func(f *fakeSandbox){
		"harness": func(f *fakeSandbox) { f.preflightCode = 1 },
		"place":   func(f *fakeSandbox) { f.preflightStartErr = fmt.Errorf("%w: boom", sandbox.ErrLaunch) },
	} {
		t.Run(kind, func(t *testing.T) {
			fakes := threeFakes()
			fail(fakes["a2"])
			runDir, err, log := runAgents(t, fakes, Options{})
			require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
			for _, a := range []string{"a1", "a3"} {
				_, serr := os.Stat(filepath.Join(runDir, a, "txx", "metadata.json"))
				require.NoError(t, serr)
			}
			_, serr := os.Stat(filepath.Join(runDir, "a2"))
			assert.That(t, os.IsNotExist(serr), "a dropped agent has a pair dir")
			assert.ContainsString(t, log, "preflight failed for a2 ("+kind+"): 1 pair(s) not started")
			assert.Equal(t, fakes["a3"].preflights, 1) // checked after the failure
		})
	}
}

// strict, or every agent failing: no pair runs, no run dir, every failure
// named.
func TestPreflightFatal(t *testing.T) {
	t.Run("strict", func(t *testing.T) {
		fakes := threeFakes()
		fakes["a1"].preflightCode, fakes["a3"].preflightCode = 1, 1
		runDir, err, _ := runAgents(t, fakes, Options{StrictPreflight: true})
		require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
		assert.ContainsString(t, err.Error(), `"a1"`)
		assert.ContainsString(t, err.Error(), `"a3"`)
		assert.Equal(t, runDir, "")
		assert.Equal(t, fakes["a2"].calls, 1) // its preflight alone
	})
	t.Run("all failed", func(t *testing.T) {
		fakes := threeFakes()
		for _, f := range fakes {
			f.preflightCode = 1
		}
		runDir, err, _ := runAgents(t, fakes, Options{})
		require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
		assert.Equal(t, runDir, "")
	})
}

// under -N the count is every repeat's.
func TestPreflightDroppedRepeats(t *testing.T) {
	fakes := threeFakes()
	fakes["a2"].preflightCode = 1
	_, err, log := runAgents(t, fakes, Options{Repeat: 3})
	require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
	assert.ContainsString(t, log, "3 pair(s) not started over 3 runs")
}

// a budget stop and a dropped agent: both lines, the budget's error.
func TestPreflightDroppedAndBudget(t *testing.T) {
	fakes := threeFakes()
	for _, f := range fakes {
		f.preRun = steps(1, 1)
	}
	fakes["a2"].preflightCode = 1
	_, err, log := runAgents(t, fakes, Options{Jobs: 1, Repeat: 3, Budget: config.Budget{Cost: 3}})
	require.That(t, errors.Is(err, ErrBudget), "not ErrBudget: %v", err)
	assert.ContainsString(t, log, "preflight failed for a2 (harness): 3 pair(s) not started over 3 runs")
	assert.ContainsString(t, log, "1 pair(s) of run 2/3 and 1 more run(s) (2 pair(s)) not started")
}

// the invocation's failure fails the run; within one check the worst kind
// wins.
func TestPreflightKinds(t *testing.T) {
	pairs := []config.TestPair{{Agent: "a"}, {Agent: "b"}}
	_, _, err := dropFailed(pairs, []agentFailure{{agent: "a", kind: failInvocation, err: errors.New("x")}}, false)
	require.That(t, errors.Is(err, ErrPreflight), "not fatal: %v", err)
	kept, n, err := dropFailed(pairs, []agentFailure{{agent: "a", kind: failPlace, err: errors.New("x")}}, false)
	require.NoError(t, err)
	assert.Equal(t, len(kept), 1)
	assert.Equal(t, n, 1)

	agent, place, inv := failed(failAgent, errors.New("h")), failed(failPlace, errors.New("p")), failed(failInvocation, errors.New("i"))
	assert.Equal(t, failOf(agent, place, inv), inv)
	assert.Equal(t, failOf(agent, place), place)
	assert.Equal(t, failOf(nil, agent), agent)
	assert.That(t, failOf(nil, nil) == nil, "no failure")
	assert.That(t, strings.Contains(agentFailure{agent: "a", kind: failPlace, err: errors.New("x")}.Error(), "(place)"), "kind not named")
}

// an interrupt still says what the preflight left out.
func TestPreflightDroppedInterrupted(t *testing.T) {
	fakes := threeFakes()
	fakes["a2"].preflightCode = 1
	fakes["a1"].onRun = func(sandbox.Spec) { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) }
	fakes["a1"].blockUntilDone = true
	_, err, log := runAgents(t, fakes, Options{Jobs: 1})
	require.That(t, errors.Is(err, ErrInterrupted), "not ErrInterrupted: %v", err)
	assert.ContainsString(t, log, "preflight failed for a2")
}
