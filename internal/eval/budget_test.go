package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// opencode's step_finish lines, each costing c.
func steps(n int, c float64) string {
	var b strings.Builder
	for range n {
		fmt.Fprintf(&b, `{"type":"step_finish","part":{"cost":%g,"tokens":{"output":1}}}`+"\n", c)
	}
	return b.String()
}

func readMeta(t *testing.T, runDir, agent, task string) pairMeta {
	b, err := os.ReadFile(filepath.Join(runDir, agent, task, "metadata.json"))
	require.NoError(t, err)
	var m pairMeta
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

// a pair whose spend passes its agent's max-cost is stopped as a timed-out
// one is: synced, collected, and its status budget, naming both amounts.
func TestAgentMaxCost(t *testing.T) {
	// the agent reports three $1 steps, then runs on until stopped.
	stepping := &fakeSandbox{blockUntilDone: true, preRun: steps(3, 1)}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return stepping, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx, max-cost: 2.5}}}
txx: {kind: task, items: {txx: {prompt: p, collect: echo collected, timeout: 1m}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	m := readMeta(t, runDir, "axx", "txx")
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "cost $3.00 passed max-cost $2.50")
	b, err := os.ReadFile(filepath.Join(runDir, "axx", "txx", "collect.log"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "collected\n")
	assert.ContainsString(t, out.String(), "axx/txx budget")
}

// with --max-cost no pair starts once the spend reached it: the rest are
// not started, and the run says so and errors.
func TestRunMaxCost(t *testing.T) {
	fake := &fakeSandbox{preRun: steps(1, 1)}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p}, t2x: {prompt: p}, t3x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, MaxCost: 1.5})
	assert.ErrorIs(t, err, ErrBudget)
	assert.ContainsString(t, out.String(), "WARN budget $1.50 reached at $2.00: 1 pair(s) not started")
	ran := 0
	for _, task := range []string{"t1x", "t2x", "t3x"} {
		if _, err := os.Stat(filepath.Join(runDir, "axx", task)); err == nil {
			ran++
		}
	}
	assert.Equal(t, ran, 2)

	// under repeats the budget is the invocation's: no further run starts.
	dir2 := t.TempDir()
	pinAll(t, dir2, cfg)
	out.Reset()
	_, err = Run(cfg, Options{ConfigDir: dir2, Now: time.Now(), Out: &out, Jobs: 1, MaxCost: 3, Repeat: 3})
	assert.ErrorIs(t, err, ErrBudget)
	runs, _ := sortedRunNames(filepath.Join(dir2, runsSubdir))
	assert.Len(t, runs, 1)
	assert.ContainsString(t, out.String(), "reached at $3.00: 0 pair(s) of run 1/3 and 2 more run(s) not started")
}

// the ledger moves a pair from live to finished in one step, counting the
// summary's cost when known, else the live spend, never both.
func TestLedger(t *testing.T) {
	l := newLedger(0)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &pairStat{}
			require.That(t, l.admit(s), "admit")
			l.add(s, 1)
			l.add(s, 1)
			if i%2 == 0 {
				s.cost, s.costKnown = 2.5, true // the summary's
			}
			l.finish(s)
		}()
	}
	wg.Wait()
	spent, _ := l.totals()
	assert.Equal(t, spent, 25*2.5+25*2.0)

	capped := newLedger(1)
	s := &pairStat{}
	require.That(t, capped.admit(s), "the first pair")
	capped.add(s, 1)
	assert.That(t, !capped.admit(&pairStat{}), "admitted past the cap")
	capped.finish(s) // no summary: the live spend stands
	spent, not := capped.totals()
	assert.Equal(t, spent, 1.0)
	assert.Equal(t, not, 1)
}
