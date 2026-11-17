package eval

import (
	"bytes"
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

func key(task string) historyKey { return historyKey{agent: "a", task: task} }

func names(queue []*queued) []string {
	var out []string
	for _, q := range queue {
		out = append(out, q.pair.Task+"#"+string(rune('0'+q.rep)))
	}
	return out
}

func queueOf(reps int, limit time.Duration, tasks ...string) []*queued {
	var q []*queued
	for r := 1; r <= reps; r++ {
		for i, t := range tasks {
			q = append(q, &queued{rep: r, idx: i, pair: config.TestPair{Agent: "a", Task: t}, key: key(t), limit: limit})
		}
	}
	return q
}

// longest first: unknown, then the longest estimate, then the earlier
// repeat, then the selection's order.
func TestDispatchOrder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	hist := map[historyKey]*pastPairs{
		key("x"): {durations: []float64{600}},
		key("y"): {durations: []float64{60, 50, 70}},
	}

	e := newEstimates(hist)
	q := queueOf(1, 0, "x", "y", "z")
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"z#1", "x#1", "y#1"})

	// a sample here replaces the history's median.
	e.end(key("x"), t0, "ok", 10)
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"z#1", "y#1", "x#1"})

	// an error is no sample.
	e = newEstimates(hist)
	e.end(key("x"), t0, "error", 1)
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"z#1", "x#1", "y#1"})

	// a pair running past its estimate raises its siblings; a key whose only
	// pair is running is no longer unknown.
	e = newEstimates(hist)
	e.start(key("y"), t0)
	e.start(key("z"), t0.Add(30*time.Second))
	q = queueOf(1, 0, "x", "y", "z")
	e.order(q, t0.Add(20*time.Minute))
	assert.EqualArrays(t, names(q), []string{"y#1", "z#1", "x#1"})
	e.end(key("y"), t0, "ok", 1200)
	e.order(q, t0.Add(20*time.Minute))
	assert.EqualArrays(t, names(q), []string{"y#1", "z#1", "x#1"})

	// capped at the timeout: x's 10m under a 5m timeout ties with y's 5m and
	// keeps the selection's order; no timeout is no cap.
	e = newEstimates(map[historyKey]*pastPairs{key("x"): {durations: []float64{600}}, key("y"): {durations: []float64{300}}})
	q = queueOf(1, 5*time.Minute, "y", "x")
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"y#1", "x#1"})
	q = queueOf(1, 0, "y", "x")
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"x#1", "y#1"})

	// the agent's time under a timeout of 0 is the cap.
	a := config.Agent{Budget: config.Budget{Time: time.Minute}}
	assert.Equal(t, effectiveTimeout(a, config.Task{Timeout: "0"}), time.Minute)
	assert.Equal(t, effectiveTimeout(config.Agent{}, config.Task{Timeout: "0"}), time.Duration(0))
	assert.Equal(t, effectiveTimeout(a, config.Task{Timeout: "30s"}), 30*time.Second)

	// the same estimate in every repeat: the earlier repeat first.
	e = newEstimates(hist)
	q = queueOf(2, 0, "y", "x")
	e.order(q, t0)
	assert.EqualArrays(t, names(q), []string{"x#1", "x#2", "y#1", "y#2"})
}

// with no history and one slot, repeat 1 goes in the selection's order,
// then what was learned ranks the rest.
func TestDispatchLearns(t *testing.T) {
	durs := map[string]time.Duration{"x": 10 * time.Minute, "y": time.Minute}
	e := newEstimates(nil)
	q := queueOf(3, 0, "x", "y")
	now := time.Unix(0, 0)
	var started []string
	for len(q) > 0 {
		e.order(q, now)
		next := q[0]
		q = q[1:]
		started = append(started, names([]*queued{next})[0])
		e.start(next.key, now)
		at := now
		now = now.Add(durs[next.pair.Task])
		e.end(next.key, at, "ok", durs[next.pair.Task].Seconds())
	}
	assert.EqualArrays(t, started, []string{"x#1", "y#1", "x#2", "x#3", "y#2", "y#3"})
}

// a run reads what history it can: what cannot be read is left out without
// a word; the dry run's reading stops on it.
func TestReadHistoryLenient(t *testing.T) {
	base := t.TempDir()
	write := func(run, agent, task, body string) {
		dir := filepath.Join(base, run, agent, task)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(body), 0o644))
	}
	write("001-20261101-a-b", "a", "x", `{"agent":"a","task":"x","status":"ok","duration_s":12}`)
	write("001-20261101-a-b", "a", "y", `{"agent":"a","task":"y","status":"error","duration_s":0}`)
	write("002-20261101-c-d", "a", "x", `{not json`)
	write("003-20261101-e-f", "a", "x", `{"agent":"a","task":"x","status":"timeout","duration_s":30}`)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "004-20261101-g-h", "a", "x", "metadata.json"), 0o755)) // unreadable as a file

	want := map[historyKey]bool{key("x"): true, key("y"): true}
	var out bytes.Buffer
	h, err := readHistory(base, want, false, logw{w: &out})
	require.NoError(t, err)
	assert.EqualArrays(t, h[key("x")].durations, []float64{12, 30})
	assert.Len(t, h[key("y")].durations, 0)
	assert.Equal(t, out.String(), "")

	_, err = readHistory(base, want, true, logw{w: &out})
	assert.Error(t, err, "metadata.json")
}

// the warning names the repeats begun with pairs not started, and those
// never begun.
func TestNotStartedText(t *testing.T) {
	for want, reps := range map[string][]repeatRun{
		"3 pair(s) of runs 1-2/3 and 1 more run(s)": {{dir: "a", notStarted: 1}, {dir: "b", notStarted: 2}, {}},
		"2 pair(s) of run 2/3 and 1 more run(s)":    {{dir: "a"}, {dir: "b", notStarted: 2}, {}},
		"2 pair(s) of run 3/3":                      {{dir: "a"}, {dir: "b"}, {dir: "c", notStarted: 2}},
		"2 more run(s)":                             {{dir: "a"}, {}, {}},
		"3 run(s)":                                  {{}, {}, {}},
	} {
		d := &dispatch{n: len(reps)}
		for i := range reps {
			d.reps = append(d.reps, &reps[i])
		}
		assert.Equal(t, d.notStartedText(), want)
	}
	assert.Equal(t, (&dispatch{n: 1, reps: []*repeatRun{{dir: "a", notStarted: 4}}}).notStartedText(), "4 pair(s)")
}

// gate holds every agent run until released, and records the prompts that
// started, in order.
type gate struct {
	mu      sync.Mutex
	started []string
	running map[string]int
	most    map[string]int // the most running at once, by prompt
	release chan struct{}
	hold    func(prompt string) bool
	// group, when set, names a prompt's group; groupMost is the most of a
	// group running at once.
	group     func(prompt string) string
	inGroup   map[string]int
	groupMost map[string]int
}

func newGate(hold func(string) bool) *gate {
	return &gate{
		running: map[string]int{}, most: map[string]int{}, release: make(chan struct{}), hold: hold,
		group: func(p string) string { return p }, inGroup: map[string]int{}, groupMost: map[string]int{},
	}
}

func (g *gate) onRun(spec sandbox.Spec) {
	p := promptOf(spec)
	g.mu.Lock()
	g.started = append(g.started, p)
	g.running[p]++
	g.most[p] = max(g.most[p], g.running[p])
	g.inGroup[g.group(p)]++
	g.groupMost[g.group(p)] = max(g.groupMost[g.group(p)], g.inGroup[g.group(p)])
	g.mu.Unlock()
	if g.hold(p) {
		<-g.release
	}
	g.mu.Lock()
	g.running[p]--
	g.inGroup[g.group(p)]--
	g.mu.Unlock()
}

func (g *gate) snapshot() ([]string, map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	most := map[string]int{}
	for k, v := range g.most {
		most[k] = v
	}
	return append([]string(nil), g.started...), most
}

// promptOf is the test prompt (p-<name>) the pair was given.
func promptOf(spec sandbox.Spec) string {
	data, _ := os.ReadFile(filepath.Join(homeMountHost(spec), promptName))
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "p-")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gatedSandbox(t *testing.T, g *gate) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		return &fakeSandbox{stdout: "fine\n", onRun: g.onRun}, nil
	}
	t.Cleanup(func() { newSandbox = old })
}

// repeats overlap: with a slow pair and a fast one, the slow pair of
// repeat 2 starts while repeat 1's is still running, and the invocation
// returns only once both end.
func TestRunRepeatsOverlap(t *testing.T) {
	g := newGate(func(p string) bool { return p == "slow" })
	gatedSandbox(t, g)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {slow: {prompt: p-slow}, fast: {prompt: p-fast}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	errc := make(chan error, 1)
	go func() {
		_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 2, Repeat: 2, Verbose: 1})
		errc <- err
	}()
	waitFor(t, "both slow pairs running", func() bool { _, most := g.snapshot(); return most["slow"] == 2 })
	select {
	case err := <-errc:
		t.Fatalf("returned with pairs running: %v", err)
	default:
	}
	close(g.release)
	require.NoError(t, <-errc)
	started, _ := g.snapshot()
	assert.Len(t, started, 4)
	runs, err := sortedRunNames(filepath.Join(dir, runsSubdir))
	require.NoError(t, err)
	require.Len(t, runs, 2)
	for _, r := range runs {
		for _, task := range []string{"slow", "fast"} {
			_, err := os.Stat(filepath.Join(dir, runsSubdir, r, "axx", task, "metadata.json"))
			assert.NoError(t, err, r+"/"+task)
		}
	}
	for _, w := range []string{"INFO axx/slow#2 ok", "INFO axx/fast#1 ok", "INFO run 1/2: 2 ok", "INFO run 2/2: 2 ok"} {
		assert.ContainsString(t, out.String(), w)
	}
	assert.ContainsString(t, out.String(), "dispatch: longest first, history for 0 of 2 pair(s)")
}

// a pair held back by its sandbox's max_parallel lets a later pair of
// another sandbox start, and the cap holds across repeats.
func TestDispatchSandboxCap(t *testing.T) {
	g := newGate(func(p string) bool { return p == "xx" || p == "yy" })
	g.group = func(p string) string {
		if p == "xx" || p == "yy" {
			return "sa"
		}
		return p
	}
	gatedSandbox(t, g)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sbx: {kind: sandbox, items: {sa: {image: s, max-parallel: 1}, sb: {image: s}}}
aa: {kind: agent, items: {aa: {harness: opencode-openrouter, model: m, sandbox: sa}}}
ab: {kind: agent, items: {ab: {harness: opencode-openrouter, model: m, sandbox: sb}}}
tx: {kind: task, items: {xx: {prompt: p-xx}, yy: {prompt: p-yy}}}
tz: {kind: task, items: {zz: {prompt: p-zz}}}
s1: {kind: suite, items: {s1: {agents: aa, tasks: tx}}}
s2: {kind: suite, items: {s2: {agents: ab, tasks: tz}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	errc := make(chan error, 1)
	go func() {
		_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 2, Repeat: 2})
		errc <- err
	}()
	waitFor(t, "both zz pairs", func() bool {
		s, _ := g.snapshot()
		return strings.Count(strings.Join(s, ","), "zz") == 2
	})
	close(g.release)
	require.NoError(t, <-errc)
	started, _ := g.snapshot()
	assert.Len(t, started, 6)
	// the agents run in their own time after the dispatcher starts them, so
	// only what ran before the release is certain, not its order.
	assert.EqualArraysUnordered(t, started[:3], []string{"xx", "zz", "zz"})
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Equal(t, g.groupMost["sa"], 1)
}

// a run dir that cannot be made stops the queue: the running pair ends,
// nothing further starts, and the invocation fails with that error.
func TestDispatchRunDirFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes through a read-only dir")
	}
	dir := t.TempDir()
	runs := filepath.Join(dir, runsSubdir)
	t.Cleanup(func() { _ = os.Chmod(runs, 0o755) })
	var once sync.Once
	g := newGate(func(string) bool { return false })
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		return &fakeSandbox{stdout: "fine\n", onRun: func(spec sandbox.Spec) {
			g.onRun(spec)
			once.Do(func() { require.NoError(t, os.Chmod(runs, 0o555)) })
		}}, nil
	}
	t.Cleanup(func() { newSandbox = old })
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p-txx}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Repeat: 3})
	assert.Error(t, err, "permission denied")
	started, _ := g.snapshot()
	assert.Len(t, started, 1)
	names, err := sortedRunNames(runs)
	require.NoError(t, err)
	require.Len(t, names, 1)
	_, err = os.Stat(filepath.Join(runs, names[0], "axx", "txx", "metadata.json"))
	assert.NoError(t, err)
}
