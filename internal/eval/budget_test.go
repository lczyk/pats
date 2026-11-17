package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
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

// a pair whose spend passes its agent's budget is stopped as a timed-out
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
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx, budget: 2.5}}}
txx: {kind: task, items: {txx: {prompt: p, collect: echo collected, timeout: 1m}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	m := readMeta(t, runDir, "axx", "txx")
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "cost $3.00 passed its budget $2.50")
	b, err := os.ReadFile(filepath.Join(runDir, "axx", "txx", "collect.log"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "collected\n")
	assert.ContainsString(t, out.String(), "axx/txx budget")
}

// a harness that reports tokens and no cost is priced by what the catalogue
// says its model costs: the pair's summary holds the estimate, marked as
// one, and the agent's budget stops the pair on it as on a reported cost.
// without a price, as offline, the pair has no cost.
func TestCostFromTokens(t *testing.T) {
	// a million input tokens and a million cached, then on until stopped.
	const usage = `{"in":1000000,"cached":1000000,"out":0}` + "\n"
	const body = `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
hxx:
  kind: harness
  items:
    tok:
      run: tok
      models: openrouter
      parse: |
        ev = json.decode(line, default = {})
        out.usage(turns = 1, input_tokens = ev.get("in", 0), cache_read_tokens = ev.get("cached", 0), output_tokens = ev.get("out", 0))
axx: {kind: agent, items: {axx: {harness: tok, model: x/y, sandbox: sxx%s}}}
txx: {kind: task, items: {txx: {prompt: p, timeout: 1m}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`
	run := func(t *testing.T, agentKeys string, box *fakeSandbox, prices map[string]*modelPrice) pairMeta {
		old := newSandbox
		newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
		defer func() { newSandbox = old }()
		oldCheck := checkModels
		checkModels = func(context.Context, []config.Agent, *http.Client) (map[string]*modelPrice, []string, error) {
			return prices, nil, nil
		}
		defer func() { checkModels = oldCheck }()
		dir := t.TempDir()
		cfg := parseCfg(t, fmt.Sprintf(body, agentKeys))
		pinAll(t, dir, cfg)
		var out bytes.Buffer
		runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
		require.NoError(t, err, "%s", out.String())
		return readMeta(t, runDir, "axx", "txx")
	}
	// $2 a million input tokens, $0.5 a million cached.
	price := map[string]*modelPrice{"axx": {in: 2e-6, out: 8e-6, cacheRead: 5e-7, cacheWrite: 2e-6}}

	m := run(t, "", &fakeSandbox{stdout: usage}, price)
	assert.Equal(t, m.Status, "ok")
	assert.NearlyEqual(t, m.Summary.CostUSD, 2.5, 1e-9)
	assert.Equal(t, m.Summary.HasCost, true)
	assert.Equal(t, m.Summary.CostEstimated, true)

	m = run(t, ", budget: 1", &fakeSandbox{blockUntilDone: true, preRun: usage, preflightOut: usage}, price)
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "cost $2.50 passed its budget $1.00")

	m = run(t, "", &fakeSandbox{stdout: usage}, nil)
	assert.Equal(t, m.Summary.HasCost, false)
	assert.Equal(t, m.Summary.CostEstimated, false)
}

// a format that reports its cost once, at its end, is stopped there: the
// line arrives while the agent still runs, and the pair is budget as any
// other over its cap.
func TestAgentMaxCostAtTheEnd(t *testing.T) {
	result := `{"type":"result","total_cost_usd":3,"num_turns":2,"result":"done"}` + "\n"
	ending := &fakeSandbox{blockUntilDone: true, preRun: result, preflightOut: result}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return ending, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: claude-cli-keyless, model: m, sandbox: sxx, budget: 2.5}}}
txx: {kind: task, items: {txx: {prompt: p, timeout: 1m}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	m := readMeta(t, runDir, "axx", "txx")
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "cost $3.00 passed its budget $2.50")
	// its preflight reported a cost, so nothing warns of a cap with nothing to check.
	assert.That(t, !strings.Contains(out.String(), "reports no cost"), "%s", out.String())
}

// a cap has nothing to check under a harness that reports no cost: the
// config loads, and the agent's preflight says so, for its own budget and
// for the run's.
func TestMaxCostNoCost(t *testing.T) {
	silent := &fakeSandbox{preRun: `{"type":"item.completed","item":{"type":"command_execution"}}` + "\n"}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return silent, nil }
	defer func() { newSandbox = old }()
	const warn = "WARN agent axx reports no cost: its budget has nothing to check"
	for name, c := range map[string]struct {
		agent string
		opts  Options
		warns bool
	}{
		"the agent's cap": {"budget: 2", Options{}, true},
		"the run's cap":   {"effort: low", Options{Budget: config.Budget{Cost: 5}}, true},
		"no cap":          {"effort: low", Options{}, false},
	} {
		dir := t.TempDir()
		cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: codex-cli-keyless, model: m, sandbox: sxx, `+c.agent+`}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
		pinAll(t, dir, cfg)
		var out bytes.Buffer
		c.opts.ConfigDir, c.opts.Now, c.opts.Out = dir, time.Now(), &out
		_, err := Run(cfg, c.opts)
		require.NoError(t, err, name)
		assert.Equal(t, strings.Contains(out.String(), warn), c.warns, "%s: %s", name, out.String())
	}
}

// an amount under a cent is not printed as none.
func TestUSD(t *testing.T) {
	for x, want := range map[float64]string{0: "$0.00", 2.5: "$2.50", 0.01: "$0.01", 0.000633: "$0.00063", 0.0005: "$0.00050", 0.0042: "$0.0042"} {
		assert.Equal(t, usd(x), want)
	}
}

// with --budget no pair starts once the spend reached it: the rest are
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
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Budget: config.Budget{Cost: 1.5}})
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
	_, err = Run(cfg, Options{ConfigDir: dir2, Now: time.Now(), Out: &out, Jobs: 1, Budget: config.Budget{Cost: 3}, Repeat: 3})
	assert.ErrorIs(t, err, ErrBudget)
	runs, _ := sortedRunNames(filepath.Join(dir2, runsSubdir))
	assert.Len(t, runs, 1)
	assert.ContainsString(t, out.String(), "reached at $3.00: 2 more run(s) not started")
}

// the ledger moves a pair from live to finished in one step, counting the
// summary's counts when known, else the live ones, never both.
func TestLedger(t *testing.T) {
	l := newLedger(config.Budget{})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &pairStat{}
			require.That(t, l.admit(s), "admit")
			l.add(s, usage{1, 10, 1})
			l.add(s, usage{1, 10, 1})
			if i%2 == 0 { // the summary's
				s.cost, s.costKnown = 2.5, true
				s.tokensUsed, s.tokensKnown = 25, true
				s.toolsUsed, s.toolsKnown = 3, true
			}
			l.finish(s)
		}()
	}
	wg.Wait()
	used, _ := l.totals()
	assert.Equal(t, used, usage{25*2.5 + 25*2.0, 25*25 + 25*20, 25*3 + 25*2})

	capped := newLedger(config.Budget{Cost: 1})
	s := &pairStat{}
	require.That(t, capped.admit(s), "the first pair")
	capped.add(s, usage{cost: 1})
	assert.That(t, !capped.admit(&pairStat{}), "admitted past the cap")
	capped.finish(s) // no summary: the live spend stands
	used, not := capped.totals()
	assert.Equal(t, used.cost, 1.0)
	assert.Equal(t, not, 1)
	assert.Equal(t, capped.reachedAt(), "budget $1.00 reached at $1.00")

	// a spend that reached the cap and later reads under it (a summary
	// below the live sum, or a float sum regrouped as pairs finish:
	// 0.01+(0.01+0.06) is 0.07999999999999999) stays reached.
	latched := newLedger(config.Budget{Cost: 0.08})
	a := &pairStat{}
	require.That(t, latched.admit(a), "admit")
	latched.add(a, usage{cost: 0.08})
	a.cost, a.costKnown = 0.07, true
	latched.finish(a)
	used, _ = latched.totals()
	require.That(t, used.cost < 0.08, "the spend did not drop: %v", used.cost)
	assert.That(t, latched.reached(), "the cap re-opened")
	assert.That(t, !latched.admit(&pairStat{}), "admitted after the cap re-opened")
}

// tokens and tools are capped as cost is; a restated total below the sum
// takes it back; one change reaching two caps latches the first kind, in
// the order cost, tokens, tools, time; the clock runs from the ledger's
// start.
func TestLedgerKinds(t *testing.T) {
	l := newLedger(config.Budget{Tokens: 100, Tools: 5})
	s := &pairStat{}
	require.That(t, l.admit(s), "admit")
	l.add(s, usage{tokens: 90, tools: 4})
	l.add(s, usage{tokens: -20})
	assert.That(t, !l.reached(), "reached at 70 tokens, 4 tools")
	l.add(s, usage{tokens: 30, tools: 1}) // both caps at once
	assert.That(t, l.reached(), "not reached")
	assert.Equal(t, l.hit, "tokens")
	assert.Equal(t, l.reachedAt(), "budget tokens 100 reached at 100")
	l.add(s, usage{tools: 10})
	assert.Equal(t, l.hit, "tokens") // latched

	now := time.Unix(1000, 0)
	timed := newLedger(config.Budget{Time: time.Hour, Cost: 5})
	timed.start, timed.now = now, func() time.Time { return now }
	require.That(t, timed.admit(&pairStat{}), "admitted at the start")
	now = now.Add(time.Hour + 13*time.Second)
	assert.That(t, !timed.admit(&pairStat{}), "admitted past the clock")
	assert.Equal(t, timed.reachedAt(), "budget time 1h0m0s reached at 1h0m13s")
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
	runDir, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 4, Budget: config.Budget{Cost: 2.5}})
	assert.ErrorIs(t, err, ErrBudget)
	n := ran(runDir)
	assert.That(t, n >= 3 && n < 8, "%d pairs ran under a budget three of them spend: %s", n, out.String())
	assert.ContainsString(t, out.String(), "pair(s) not started")
}

// runOne runs agent axx on task txx under box, the agent and task taking
// the extra keys given, and returns the pair's metadata and the log.
func runOne(t *testing.T, harness, agentKeys, taskKeys string, box *fakeSandbox, opts Options) (pairMeta, string) {
	t.Helper()
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: `+harness+`, model: m, sandbox: sxx, `+agentKeys+`}}}
txx: {kind: task, items: {txx: {prompt: p, collect: echo collected, `+taskKeys+`}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	opts.ConfigDir, opts.Now, opts.Out = dir, time.Now(), &out
	runDir, err := Run(cfg, opts)
	require.NoError(t, err, "%s", out.String())
	return readMeta(t, runDir, "axx", "txx"), out.String()
}

// opencode's tool_use lines.
func toolUses(n int) string {
	return strings.Repeat(`{"type":"tool_use","part":{"tool":"read"}}`+"\n", n)
}

// a tokens or tools cap stops a pair mid-stream as a cost cap does:
// synced, collected, status budget, the kind in budget_kind and the error.
func TestAgentBudgetKinds(t *testing.T) {
	// three steps of 1000 tokens each: 400 in, 300 out, 200 cache read, 100 cache write.
	step := `{"type":"step_finish","part":{"cost":0.01,"tokens":{"input":400,"output":300,"cache":{"read":200,"write":100}}}}` + "\n"
	for name, c := range map[string]struct {
		agent, preRun, kind, err string
	}{
		"tokens":    {"budget: {tokens: 2.5k}", strings.Repeat(step, 3), "tokens", "tokens 3000 passed its budget 2500"},
		"tools":     {"budget: {tools: 3}", toolUses(4), "tools", "tools 4 passed its budget 3"},
		"cost wins": {"budget: {cost: 0.025, tokens: 2.5k}", strings.Repeat(step, 3), "cost", "cost $0.03 passed its budget $0.03"},
	} {
		box := &fakeSandbox{blockUntilDone: true, preRun: c.preRun}
		m, out := runOne(t, "opencode-openrouter", c.agent, "timeout: 1m", box, Options{})
		assert.Equal(t, m.Status, "budget", name)
		assert.Equal(t, m.BudgetKind, c.kind, name)
		assert.Equal(t, m.Error, c.err, name)
		assert.ContainsString(t, out, "axx/txx budget")
	}
}

// claude states its usage on its final result only: a tokens cap marks
// the pair there.
func TestAgentTokensAtTheEnd(t *testing.T) {
	result := `{"type":"result","total_cost_usd":0.1,"num_turns":2,"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":3000,"cache_creation_input_tokens":0},"result":"done"}` + "\n"
	box := &fakeSandbox{blockUntilDone: true, preRun: result, preflightOut: result}
	m, out := runOne(t, "claude-cli-keyless", "budget: {tokens: 3000}", "timeout: 1m", box, Options{})
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "tokens 3030 passed its budget 3000")
	assert.That(t, !strings.Contains(out, "reports no tokens"), "%s", out)
}

// an agent's time below the task's effective timeout stops the pair as a
// budget; at or above it the task's deadline binds; under a timeout of 0
// the agent's time is the only one.
func TestAgentTime(t *testing.T) {
	for name, c := range map[string]struct {
		agent, task, status, kind string
	}{
		"below the timeout": {"budget: {time: 50ms}", "timeout: 1m", "budget", "time"},
		"equal":             {"budget: {time: 100ms}", "timeout: 100ms", "timeout", ""},
		"no timeout":        {"budget: {time: 50ms}", `timeout: "0"`, "budget", "time"},
	} {
		box := &fakeSandbox{blockUntilDone: true}
		m, _ := runOne(t, "opencode-openrouter", c.agent, c.task, box, Options{})
		assert.Equal(t, m.Status, c.status, name)
		assert.Equal(t, m.BudgetKind, c.kind, name)
		if c.kind == "time" {
			assert.That(t, strings.HasPrefix(m.Error, "time ") && strings.HasSuffix(m.Error, " reached its budget 50ms"), "%s: %s", name, m.Error)
		}
	}
}

// --budget tools=1: the first pair's one tool call reaches it, and the rest
// do not start.
func TestRunBudgetTools(t *testing.T) {
	fake := &fakeSandbox{preRun: toolUses(1)}
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
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Budget: config.Budget{Tools: 1}})
	assert.ErrorIs(t, err, ErrBudget)
	assert.ContainsString(t, out.String(), "WARN budget tools 1 reached at 1: 2 pair(s) not started")
}

// a cap of a kind the harness reports nothing of warns after preflight:
// tokens when preflight saw no token count, tools when the harness has no
// parse script.
func TestBudgetKindsNothingToCheck(t *testing.T) {
	silent := &fakeSandbox{preRun: `{"type":"item.completed","item":{"type":"command_execution"}}` + "\n"}
	_, out := runOne(t, "codex-cli-keyless", "budget: {tokens: 1M, tools: 10}", "", silent, Options{})
	assert.ContainsString(t, out, "WARN agent axx reports no tokens: its tokens budget has nothing to check")
	assert.That(t, !strings.Contains(out, "reports no tool calls"), "%s", out)

	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return &fakeSandbox{}, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
hxx: {kind: harness, items: {bare: {run: "true"}}}
axx: {kind: agent, items: {axx: {harness: bare, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var buf bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &buf, Budget: config.Budget{Tools: 5}})
	require.NoError(t, err, "%s", buf.String())
	assert.ContainsString(t, buf.String(), "WARN agent axx reports no tool calls: its tools budget has nothing to check")
}

// an agent that never launched is an error, whatever its time budget did
// meanwhile: nothing ran, so nothing is collected.
func TestAgentTimeLaunchFailure(t *testing.T) {
	box := &fakeSandbox{blockUntilDone: true, err: fmt.Errorf("start: %w", sandbox.ErrLaunch)}
	m, _ := runOne(t, "opencode-openrouter", "budget: {time: 20ms}", "timeout: 1m", box, Options{})
	assert.Equal(t, m.Status, "error")
	assert.Equal(t, m.BudgetKind, "")
}

// tok reports the total tokens each line states.
const totalTokens = `
hxx:
  kind: harness
  items:
    tok:
      run: tok
      parse: |
        ev = json.decode(line, default = {})
        out.usage(total = True, input_tokens = ev.get("t", 0))
`

// the stop says what the pair had when it passed its cap: a later line
// restating the total lower does not change it.
func TestAgentStopSnapshot(t *testing.T) {
	old := newSandbox
	box := &fakeSandbox{blockUntilDone: true, preRun: `{"t":110}` + "\n" + `{"t":90}` + "\n"}
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, totalTokens+`
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: tok, sandbox: sxx, budget: {tokens: 100}}}}
txx: {kind: task, items: {txx: {prompt: p, timeout: 1m}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, "%s", out.String())
	m := readMeta(t, runDir, "axx", "txx")
	assert.Equal(t, m.Status, "budget")
	assert.Equal(t, m.Error, "tokens 110 passed its budget 100")
	assert.Equal(t, m.Summary.InputTokens, 90)
}

// a last line with no newline is the summary's alone: a cap it passes
// changes no status, and counts toward the run's cap from then on.
func TestBudgetFromTheSummary(t *testing.T) {
	old := newSandbox
	box := &fakeSandbox{preRun: `{"t":50}` + "\n", stdout: `{"t":150}`, preflightOut: `{"t":1}` + "\n"}
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, totalTokens+`
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: tok, sandbox: sxx, budget: {tokens: 100}}}}
tvx: {kind: task, items: {t1x: {prompt: p}, t2x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Budget: config.Budget{Tokens: 100}})
	assert.ErrorIs(t, err, ErrBudget)
	ran := "t1x" // -j 1 runs one pair, whichever takes the slot first
	if _, err := os.Stat(filepath.Join(runDir, "axx", ran)); err != nil {
		ran = "t2x"
	}
	m := readMeta(t, runDir, "axx", ran)
	assert.Equal(t, m.Status, "ok")
	assert.Equal(t, m.Summary.InputTokens, 150)
	assert.ContainsString(t, out.String(), "WARN budget tokens 100 reached at 150: 1 pair(s) not started")
}
