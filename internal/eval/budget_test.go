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

// behind the machine's proxy the run names the upstream, redacted, before
// its first pair.
func TestRunNamesUpstream(t *testing.T) {
	fake := &fakeSandbox{}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()
	oldMP := machineProxy
	machineProxy = func() (*sandbox.MachineProxy, error) {
		return sandbox.ReadMachineProxy(func(k string) string {
			return map[string]string{"HTTPS_PROXY": "http://u:secret@proxy.corp:3128"}[k]
		})
	}
	defer func() { machineProxy = oldMP }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1})
	require.NoError(t, err)
	text := out.String()
	i := strings.Index(text, "egress via upstream http://u:xxxxx@proxy.corp:3128 (https only)")
	assert.That(t, i >= 0, "no start line in %q", text)
	assert.That(t, i < strings.Index(text, "axx/t1x"), "start line after the first pair: %q", text)
	assert.That(t, !strings.Contains(text, "secret"), "credential in the output: %q", text)
}

// pairs run several at once, under -j and a sandbox's max-parallel: each
// still ends with its own metadata, and a run-wide budget stops the ones
// not yet started once those running have spent it.
func TestRunParallel(t *testing.T) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		return &fakeSandbox{preRun: steps(1, 1)}, nil
	}
	defer func() { newSandbox = old }()
	cfg := parseCfg(t, `
svx: {kind: sandbox, items: {sxx: {image: s, max-parallel: 2}, syy: {image: s}}}
avx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}, ayy: {harness: opencode-openrouter, model: m, sandbox: syy}}}
tvx: {kind: task, items: {t1x: {prompt: p}, t2x: {prompt: p}, t3x: {prompt: p}, t4x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: avx, tasks: tvx}}}
`)
	ran := func(runDir string) (n int) {
		for _, agent := range []string{"axx", "ayy"} {
			for _, task := range []string{"t1x", "t2x", "t3x", "t4x"} {
				if _, err := os.Stat(filepath.Join(runDir, agent, task)); err == nil {
					n++
					assert.Equal(t, readMeta(t, runDir, agent, task).Status, "ok", "%s/%s", agent, task)
				}
			}
		}
		return n
	}
	dir := t.TempDir()
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 4})
	require.NoError(t, err, "%s", out.String())
	assert.Equal(t, ran(runDir), 8)

	dir = t.TempDir()
	pinAll(t, dir, cfg)
	out.Reset()
	runDir, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 4, MaxCost: 2.5})
	assert.ErrorIs(t, err, ErrBudget)
	n := ran(runDir)
	assert.That(t, n >= 3 && n < 8, "%d pairs ran under a budget three of them spend: %s", n, out.String())
	assert.ContainsString(t, out.String(), "pair(s) not started")
}
