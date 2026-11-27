package eval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// a pair's setup runs while its agent's preflight is held, and its agent
// starts only once the preflight has passed; the wait is its gate_s.
func TestOverlapSetupBeforeGate(t *testing.T) {
	hold := make(chan struct{})
	fakes := threeFakes()
	var setUp, agentAt atomic.Int64
	for _, f := range fakes {
		f.preflightHold = hold
	}
	fakes["a1"].onRun = func(sandbox.Spec) { agentAt.Store(time.Now().UnixNano()) }
	// the pair's setup has started a session once its Start was called:
	// count the non-preflight starts.
	go func() {
		for {
			fakes["a1"].mu.Lock()
			n := fakes["a1"].starts
			fakes["a1"].mu.Unlock()
			if n > 0 {
				setUp.Store(time.Now().UnixNano())
				time.Sleep(50 * time.Millisecond)
				close(hold)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	runDir, err, _ := runAgents(t, fakes, Options{Jobs: 3})
	require.NoError(t, err)
	require.That(t, setUp.Load() > 0 && agentAt.Load() > setUp.Load(), "the agent ran before the pair's setup, or setup never ran")
	meta, err := os.ReadFile(filepath.Join(runDir, "a1", "txx", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"gate_s"`)
}

// a preflight that fails fatally (strict): no agent ran, no run dir is
// left, and the next run takes the number; under -N none of three.
func TestOverlapFatal(t *testing.T) {
	fakes := threeFakes()
	fakes["a2"].preflightCode = 1
	runDir, err, _ := runAgents(t, fakes, Options{StrictPreflight: true, Repeat: 3, Jobs: 3})
	require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
	assert.Equal(t, runDir, "")
	for _, f := range fakes {
		assert.Equal(t, f.agentRuns(), 0)
	}
}

// a dropped agent's pairs, set up beside the preflight, leave no pair dir
// and nothing in the totals; the others run.
func TestOverlapDropped(t *testing.T) {
	fakes := threeFakes()
	fakes["a2"].preflightCode = 1
	runDir, err, log := runAgents(t, fakes, Options{Jobs: 3})
	require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
	_, serr := os.Stat(filepath.Join(runDir, "a2"))
	assert.That(t, os.IsNotExist(serr), "a dropped agent has a pair dir")
	assert.Equal(t, fakes["a2"].agentRuns(), 0)
	assert.Equal(t, fakes["a1"].agentRuns(), 1)
	assert.That(t, !strings.Contains(log, "a2/txx error"), "a dropped pair said a verdict")
}

// a setup that fails while the preflight runs is the pair's error once
// the gate opens; a dropped agent's failed setup is discarded.
func TestOverlapSetupFails(t *testing.T) {
	fakes := threeFakes()
	fakes["a1"].startErr = errors.New("boom")
	fakes["a2"].startErr = errors.New("boom")
	fakes["a2"].preflightCode = 1
	runDir, _, _ := runAgents(t, fakes, Options{Jobs: 3})
	meta, err := os.ReadFile(filepath.Join(runDir, "a1", "txx", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "error"`)
	_, serr := os.Stat(filepath.Join(runDir, "a2"))
	assert.That(t, os.IsNotExist(serr), "a dropped agent's failed setup was recorded")
}

// an interrupt before the gate opens: no run dir, the interrupt's error.
func TestOverlapInterruptBeforeGate(t *testing.T) {
	hold := make(chan struct{})
	fakes := threeFakes()
	for _, f := range fakes {
		f.preflightHold = hold
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	runDir, err, _ := runAgents(t, fakes, Options{Jobs: 3})
	require.That(t, errors.Is(err, ErrInterrupted), "not ErrInterrupted: %v", err)
	assert.Equal(t, runDir, "")
	for _, f := range fakes {
		assert.Equal(t, f.agentRuns(), 0)
	}
}

// the run's time budget counts from the gate: a preflight longer than it
// stops nothing.
func TestOverlapBudgetFromGate(t *testing.T) {
	hold := make(chan struct{})
	fakes := threeFakes()
	for _, f := range fakes {
		f.preflightHold = hold
	}
	go func() { time.Sleep(150 * time.Millisecond); close(hold) }()
	_, err, _ := runAgents(t, fakes, Options{Jobs: 1, Budget: config.Budget{Time: 100 * time.Millisecond}})
	require.NoError(t, err)
	for _, f := range fakes {
		assert.Equal(t, f.agentRuns(), 1)
	}
}

// a dropped agent's setup that is still running is ended at the gate, as
// is every setup when the preflight is fatal.
func TestOverlapCancelsSetup(t *testing.T) {
	for name, opts := range map[string]Options{"dropped": {Jobs: 3}, "fatal": {Jobs: 3, StrictPreflight: true}} {
		t.Run(name, func(t *testing.T) {
			fakes := threeFakes()
			fakes["a2"].preflightCode = 1
			fakes["a2"].startBlock = true
			if name == "fatal" {
				fakes["a1"].startBlock = true
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err, _ := runAgents(t, fakes, opts)
				assert.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("a dropped setup held the run")
			}
		})
	}
}

// a pair discarded at the gate closes its session once.
func TestOverlapDiscardClosesOnce(t *testing.T) {
	fakes := threeFakes()
	hold := make(chan struct{})
	fakes["a2"].preflightHold = hold
	fakes["a2"].preflightCode = 1
	go func() {
		for fakes["a2"].startCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		close(hold)
	}()
	_, err, _ := runAgents(t, fakes, Options{Jobs: 3})
	require.That(t, errors.Is(err, ErrPreflight), "not ErrPreflight: %v", err)
	fakes["a2"].mu.Lock()
	defer fakes["a2"].mu.Unlock()
	assert.Equal(t, fakes["a2"].closes, 1)
}

// a pair that starts after the gate opened has no gate_s; one that waited
// has it.
func TestOverlapLateNoGate(t *testing.T) {
	fakes := threeFakes()
	hold := make(chan struct{})
	for _, f := range fakes {
		f.preflightHold = hold
	}
	go func() {
		for fakes["a1"].startCount()+fakes["a2"].startCount()+fakes["a3"].startCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		close(hold)
	}()
	runDir, err, _ := runAgents(t, fakes, Options{Jobs: 1})
	require.NoError(t, err)
	with, without := 0, 0
	for _, a := range []string{"a1", "a2", "a3"} {
		b, err := os.ReadFile(filepath.Join(runDir, a, "txx", "metadata.json"))
		require.NoError(t, err)
		if strings.Contains(string(b), `"gate_s"`) {
			with++
		} else {
			without++
		}
	}
	assert.Equal(t, with, 1)
	assert.Equal(t, without, 2)
}

// an interrupt after the gate opened keeps the run dir.
func TestOverlapInterruptAfterGate(t *testing.T) {
	fakes := threeFakes()
	fakes["a1"].blockUntilDone = true
	fakes["a1"].onRun = func(sandbox.Spec) { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) }
	runDir, err, _ := runAgents(t, fakes, Options{Jobs: 3})
	require.That(t, errors.Is(err, ErrInterrupted), "not ErrInterrupted: %v", err)
	require.That(t, runDir != "", "no run dir")
	_, serr := os.Stat(filepath.Join(runDir, "a1", "txx", "metadata.json"))
	assert.NoError(t, serr)
}

// a stop before the gate settles still cancels a dropped agent's setup
// when it does: a run dir that cannot be made does not hold the run.
func TestOverlapStopBeforeGate(t *testing.T) {
	old := makeRunDir
	var made atomic.Int32
	makeRunDir = func(base string, now time.Time) (string, error) {
		if made.Add(1) > 1 {
			return "", errors.New("no room")
		}
		return old(base, now)
	}
	t.Cleanup(func() { makeRunDir = old })
	hold := make(chan struct{})
	fakes := threeFakes()
	for _, f := range fakes {
		f.preflightHold = hold
	}
	fakes["a2"].preflightCode = 1
	fakes["a2"].startBlock = true
	go func() { time.Sleep(100 * time.Millisecond); close(hold) }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err, _ := runAgents(t, fakes, Options{Jobs: 6, Repeat: 2})
		assert.That(t, err != nil && strings.Contains(err.Error(), "no room"), "not the run dir's error: %v", err)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a dropped setup held the stopped run")
	}
}

// neither the setup's timeout nor the task's covers the wait at the gate:
// a preflight longer than both times out nothing.
func TestOverlapTimeoutsExcludeGate(t *testing.T) {
	old := setupTimeout
	setupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { setupTimeout = old })
	hold := make(chan struct{})
	fake := &fakeSandbox{preflightHold: hold}
	oldBox := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	t.Cleanup(func() { newSandbox = oldBox })
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p, prepare: "true", timeout: 100ms}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	go func() { time.Sleep(300 * time.Millisecond); close(hold) }()
	var out strings.Builder
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1})
	require.NoError(t, err)
	meta, err := os.ReadFile(filepath.Join(runDir, "axx", "txx", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "ok"`)
	assert.ContainsString(t, string(meta), `"gate_s"`)
}
