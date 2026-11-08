package eval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
)

// score phase, no docker: an exec scorer reads each pair's stdout.log and emits
// a float; pats aggregates per (agent,task) then per agent.
func TestScoreExec(t *testing.T) {
	dir := t.TempDir()
	// scorer: 1.0 if the output contains "good", else 0.0.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "has-good.sh"),
		[]byte("#!/bin/sh\ngrep -q good \"$PATS_OUTPUT_DIR/stdout.log\" && echo 1.0 || echo 0.0\n"), 0o755))

	// fake run dir: agent a ran two tasks, one good one bad.
	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	for task, body := range map[string]string{"t1": "this is good", "t2": "this is bad"} {
		od := filepath.Join(run, "a", task)
		require.NoError(t, os.MkdirAll(od, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte(body), 0o644))
	}

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t1: ~, t2: ~}}
scorers: {kind: scorer, items: {has-good: {score: ./has-good.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t1, t2], scorers: [has-good]}}}
`)

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)

	assert.Equal(t, rep.PerPair["a/t1"], 1.0)
	assert.Equal(t, rep.PerPair["a/t2"], 0.0)
	assert.Equal(t, rep.PerAgent["a"], 0.5) // mean of the two tasks
	assert.Equal(t, rep.Overall, 0.5)

	// scores.json written into the run dir.
	_, err = os.Stat(filepath.Join(run, "scores.json"))
	require.NoError(t, err)
}

// scorers get PATS_RUN_ID (the run dir's basename), PATS_RUN_DIR (the dir
// holding every pair's output) and PATS_SCORER_VECTOR.
func TestScoreExecRunEnv(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run-env.sh"),
		[]byte("#!/bin/sh\n[ \"$PATS_RUN_ID\" = \"$(basename \"$PATS_RUN_DIR\")\" ] && [ -d \"$PATS_RUN_DIR/a/t1\" ] && [ \"$PATS_SCORER_VECTOR\" = scorers ] && echo 1.0 || echo 0.0\n"), 0o755))
	run := filepath.Join(dir, ".pats", "runs", "001-20260621-fluffy-bunny")
	require.NoError(t, os.MkdirAll(filepath.Join(run, "a", "t1"), 0o755))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t1: ~}}
scorers: {kind: scorer, items: {run-env: {score: ./run-env.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t1], scorers: [run-env]}}}
`)
	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)
	assert.Equal(t, rep.PerPair["a/t1"], 1.0)
}

// scorers get PATS_AGENT_HARNESS alongside PATS_MODEL (README documents both).
func TestScorerEnvIncludesAgentKind(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "check-harness.sh"),
		[]byte("#!/bin/sh\n[ \"$PATS_AGENT_HARNESS\" = \"opencode-openrouter\" ] && echo 1.0 || echo 0.0\n"), 0o755))

	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	od := filepath.Join(run, "a", "t")
	require.NoError(t, os.MkdirAll(od, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t: ~}}
scorers: {kind: scorer, items: {check-harness: {score: ./check-harness.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t], scorers: [check-harness]}}}
`)

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)
	assert.Equal(t, rep.PerPair["a/t"], 1.0)
}

// COVER: -j-1 during scoring must not ask a driver for its cpus -- the score
// phase never runs a container.
func TestScoreAutoJobsSkipsDocker(t *testing.T) {
	probed := false
	old := hostFor
	hostFor = func(config.Sandbox) (host, error) { probed = true; return newFakeHost(), nil }
	defer func() { hostFor = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.sh"), []byte("#!/bin/sh\necho 1.0\n"), 0o755))

	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	od := filepath.Join(run, "a", "t")
	require.NoError(t, os.MkdirAll(od, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t: ~}}
scorers: {kind: scorer, items: {one: {score: ./one.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t], scorers: [one]}}}
`)

	var out bytes.Buffer
	_, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Jobs: -1, Out: &out})
	require.NoError(t, err)
	assert.That(t, !probed, "score must not probe docker for its auto job count")
}

// a stray directory under .pats/runs (not <nnn>-<yyyymmdd>-<adj>-<noun>) must
// not be treated as a run: not picked as "latest", not listed by `pats list runs`.
func TestSortedRunNamesSkipsStrayDirs(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, runsSubdir)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "001-20260701-fluffy-bunny"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "stray"), 0o755)) // e.g. leftover from an interrupted run

	got, err := latestRunDir(base)
	require.NoError(t, err)
	assert.Equal(t, got, filepath.Join(base, "001-20260701-fluffy-bunny"))

	var out bytes.Buffer
	require.NoError(t, ListRuns(dir, &out))
	s := out.String()
	assert.ContainsString(t, s, "001-20260701-fluffy-bunny")
	assert.That(t, !strings.Contains(s, "stray"), "stray dir not listed", s)
}

// per-pair score is the plain mean over the scorers run on that pair.
func TestScoreMultipleScorers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.sh"), []byte("#!/bin/sh\necho 1.0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zero.sh"), []byte("#!/bin/sh\necho 0.0\n"), 0o755))

	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	od := filepath.Join(run, "a", "t")
	require.NoError(t, os.MkdirAll(od, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t: ~}}
scorers: {kind: scorer, items: {one: {score: ./one.sh}, zero: {score: ./zero.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t], scorers: [one, zero]}}}
`)

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)
	// (1.0 + 0.0) / 2 = 0.5
	assert.Equal(t, rep.PerPair["a/t"], 0.5)
}

// -a / -t / -s narrow the cells scored; they intersect, and a combination
// that leaves no cell is an error rather than an empty scores.json.
func TestScoreFilters(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.sh"), []byte("#!/bin/sh\necho 1.0\n"), 0o755))
	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	for _, p := range []string{"a/t1", "a/t2", "b/t1", "b/t2"} {
		od := filepath.Join(run, p)
		require.NoError(t, os.MkdirAll(od, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))
	}
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
ag: {kind: agent, defaults: {harness: opencode-openrouter, model: m, sandbox: s}, items: {a: ~, b: ~}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t1: ~, t2: ~}}
scorers: {kind: scorer, defaults: {score: ./one.sh}, items: {x: ~, y: ~}}
suites:
  kind: suite
  items:
    all: {agents: ag, tasks: tasks, scorers: x}
    only-t2: {agents: ag, tasks: t2, scorers: y}
    run-only: {agents: ag, tasks: t1}
`)
	// which cells a selection computes: no earlier scores.json to merge into.
	cells := func(o ScoreOptions) []string {
		t.Helper()
		_ = os.Remove(filepath.Join(run, "scores.json"))
		o.ConfigDir, o.RunDir, o.Out = dir, run, &bytes.Buffer{}
		rep, err := Score(cfg, o)
		require.NoError(t, err)
		var got []string
		for _, c := range rep.Cells {
			got = append(got, c.Agent+"/"+c.Task+"/"+c.Scorer)
		}
		return got
	}

	assert.Len(t, cells(ScoreOptions{}), 6) // 4 pairs x x, plus a/t2 and b/t2 x y
	assert.EqualArraysUnordered(t, cells(ScoreOptions{Scorers: []string{"y"}}), []string{"a/t2/y", "b/t2/y"})
	assert.EqualArraysUnordered(t, cells(ScoreOptions{Agents: []string{"a"}, Scorers: []string{"x"}}),
		[]string{"a/t1/x", "a/t2/x"})
	assert.EqualArraysUnordered(t, cells(ScoreOptions{Tasks: []string{"t1"}}), []string{"a/t1/x", "b/t1/x"})
	assert.EqualArraysUnordered(t, cells(ScoreOptions{Suites: []string{"only-t2"}, Agents: []string{"b"}}),
		[]string{"b/t2/y"})

	// each filter alone matches, together they leave nothing.
	_, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &bytes.Buffer{},
		Tasks: []string{"t1"}, Scorers: []string{"y"}})
	assert.Error(t, err, "nothing to score")

	// filters that match the config but not this run's outputs, and a suite
	// with no scorers, must not rewrite scores.json empty.
	_, err = Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &bytes.Buffer{}, Suites: []string{"run-only"}})
	assert.Error(t, err, "nothing to score")
	require.NoError(t, os.RemoveAll(filepath.Join(run, "b")))
	_, err = Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &bytes.Buffer{}, Agents: []string{"b"}})
	assert.Error(t, err, "nothing to score")

	// -r all skips a run that lacks the selection instead of failing on it, and
	// fails only when no run has it.
	run2 := filepath.Join(dir, ".pats", "runs", "20260622-1")
	require.NoError(t, os.MkdirAll(filepath.Join(run2, "b", "t1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(run2, "b", "t1", "stdout.log"), []byte("x"), 0o644))
	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: "all", Out: &out, Agents: []string{"b"}})
	require.NoError(t, err)
	assert.Equal(t, rep.RunDir, run2)
	assert.ContainsString(t, out.String(), "20260621-1: nothing to score")
	require.NoError(t, os.RemoveAll(filepath.Join(run2, "b")))
	_, err = Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: "all", Out: &bytes.Buffer{}, Agents: []string{"b"}})
	assert.Error(t, err, "nothing to score in any run")

	_, err = Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &bytes.Buffer{}, Scorers: []string{"nope"}})
	assert.Error(t, err, `--scorer "nope"`)
	_, err = Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &bytes.Buffer{}, Agents: []string{"nope"}})
	assert.Error(t, err, `--agent "nope"`)
}

// a narrowed score folds its cells into scores.json: what it recomputed is
// replaced, the rest is kept, and a failed narrowed score leaves the file be.
func TestScoreNarrowedMerges(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	write("x.sh", "echo 1.0")
	write("y.sh", "echo 0.0")
	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	for _, p := range []string{"a/t1", "b/t1"} {
		require.NoError(t, os.MkdirAll(filepath.Join(run, p), 0o755))
	}
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
ag: {kind: agent, defaults: {harness: opencode-openrouter, model: m, sandbox: s}, items: {a: ~, b: ~}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t1: ~}}
scorers: {kind: scorer, items: {x: {score: ./x.sh}, y: {score: ./y.sh}}}
suites: {kind: suite, items: {all: {agents: ag, tasks: tasks, scorers: scorers}}}
`)
	score := func(o ScoreOptions) (*ScoreReport, error) {
		o.ConfigDir, o.RunDir, o.Out = dir, run, &bytes.Buffer{}
		return Score(cfg, o)
	}
	cellMap := func(r *ScoreReport) map[string]float64 {
		m := map[string]float64{}
		for _, c := range r.Cells {
			m[c.Agent+"/"+c.Task+"/"+c.Scorer] = c.Score
		}
		return m
	}

	rep, err := score(ScoreOptions{})
	require.NoError(t, err)
	assert.Len(t, rep.Cells, 4)

	// y fixed: rescoring only y keeps x's cells and the means cover all four.
	write("y.sh", "echo 1.0")
	rep, err = score(ScoreOptions{Scorers: []string{"y"}})
	require.NoError(t, err)
	assert.EqualMaps(t, cellMap(rep), map[string]float64{"a/t1/x": 1, "a/t1/y": 1, "b/t1/x": 1, "b/t1/y": 1})
	assert.Equal(t, rep.Overall, 1.0)

	// a recomputed cell that now fails is dropped, not kept stale.
	write("x.sh", "exit 1")
	rep, err = score(ScoreOptions{Agents: []string{"a"}, Scorers: []string{"x"}})
	require.NoError(t, err)
	assert.EqualMaps(t, cellMap(rep), map[string]float64{"a/t1/y": 1, "b/t1/x": 1, "b/t1/y": 1})

	// the file on disk is the merged report.
	data, err := os.ReadFile(filepath.Join(run, "scores.json"))
	require.NoError(t, err)
	var onDisk ScoreReport
	require.NoError(t, json.Unmarshal(data, &onDisk))
	assert.Len(t, onDisk.Cells, 3)

	// a corrupt scores.json stops a narrowed score instead of being replaced.
	require.NoError(t, os.WriteFile(filepath.Join(run, "scores.json"), []byte(`{"cells": [`), 0o644))
	_, err = score(ScoreOptions{Scorers: []string{"y"}})
	assert.Error(t, err, "scores.json")
	got, err := os.ReadFile(filepath.Join(run, "scores.json"))
	require.NoError(t, err)
	assert.Equal(t, string(got), `{"cells": [`)
	require.NoError(t, os.WriteFile(filepath.Join(run, "scores.json"), data, 0o644))

	// a narrowed score that reaches nothing leaves scores.json as it was.
	require.NoError(t, os.RemoveAll(filepath.Join(run, "b")))
	_, err = score(ScoreOptions{Agents: []string{"b"}})
	assert.Error(t, err, "nothing to score")
	after, err := os.ReadFile(filepath.Join(run, "scores.json"))
	require.NoError(t, err)
	assert.Equal(t, string(after), string(data))
}

func TestParseScore(t *testing.T) {
	// first non-empty line is the verdict; later lines ignored.
	good := map[string]float64{"1.0\n": 1.0, "0\n": 0.0, "  0.5  ": 0.5, "0.25\nnoise\n": 0.25}
	for in, want := range good {
		got, err := parseScore(in)
		require.NoError(t, err)
		assert.Equal(t, got, want)
	}
	// "na" (any case) -> silent-skip sentinel.
	for _, na := range []string{"na", "NA\n", " Na \n0.9\n"} {
		_, err := parseScore(na)
		assert.ErrorIs(t, err, errScorerNA)
	}
	for _, bad := range []string{"", "nope", "1.5", "-0.1"} {
		_, err := parseScore(bad)
		assert.Error(t, err, assert.AnyError)
	}
}

func TestReportPivot(t *testing.T) {
	r := &ScoreReport{
		Cells: []ScoreCell{
			{Agent: "a1", Task: "t1", Scorer: "s1", Score: 1.0},
			{Agent: "a1", Task: "t2", Scorer: "s1", Score: 0.5},
			{Agent: "a1", Task: "t2", Scorer: "s2", Score: 0.0},
			{Agent: "a2", Task: "t1", Scorer: "s1", Score: 0.9},
			// a2/t2 missing -> dim dash cell
		},
		PerPair:  map[string]float64{"a1/t1": 1.0, "a1/t2": 0.25, "a2/t1": 0.9},
		PerAgent: map[string]float64{"a1": 0.625, "a2": 0.9},
		Overall:  0.7625,
	}
	var buf bytes.Buffer
	report(&buf, r, false)
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	assert.ContainsString(t, lines[0], "a1")
	assert.ContainsString(t, lines[0], "a2")
	// worst-first: t2 (0.25) row before t1.
	assert.ContainsString(t, lines[1], "t2")
	assert.ContainsString(t, lines[1], "0.25 [##-----]")
	assert.ContainsString(t, lines[1], "-") // missing a2/t2 cell
	assert.ContainsString(t, lines[2], "t1")
	assert.ContainsString(t, lines[2], "1.00 [#######]")
	assert.ContainsString(t, lines[2], "0.90 [######-]")
	assert.ContainsString(t, lines[3], "avg")
	assert.ContainsString(t, lines[4], "overall")
	assert.ContainsString(t, lines[4], "0.76")

	// breakdown covers every pair, worst first; deviants indented per line,
	// worst first (no mode tally here -- each pair's scores are all distinct).
	assert.ContainsString(t, out, "scorer breakdown:")
	assert.ContainsString(t, out, "a1/t2")
	assert.ContainsString(t, out, "\n    s2=0.00\n    s1=0.50\n")
	assert.ContainsString(t, out, "a1/t1") // perfect pairs listed too
	assert.ContainsString(t, out, "\n    s1=1.00\n")

	// no ansi when color off; ansi present when on.
	assert.That(t, !strings.Contains(out, "\033["), "no ansi in plain output")
	buf.Reset()
	report(&buf, r, true)
	assert.ContainsString(t, buf.String(), "\033[32m")
	assert.ContainsString(t, buf.String(), "\033[31m")
}

// numeric -r selectors: positive = run number, 0 = latest, negative = from
// the latest backwards; out-of-range and missing numbers are errors.
func TestResolveRunNumber(t *testing.T) {
	base := t.TempDir()
	runs := []string{
		"001-20260701-fluffy-bunny",
		"002-20260701-jacquard-ribbons",
		"003-20260702-undyed-purse",
		"004-20260702-variegated-cuff",
	}
	for _, r := range runs {
		require.NoError(t, os.Mkdir(filepath.Join(base, r), 0o755))
	}

	for arg, want := range map[string]string{
		"1":  runs[0], // run number...
		"01": runs[0], // ...zero-padding immaterial
		"3":  runs[2],
		"0":  runs[3], // latest (the "" default)
		"-1": runs[2], // second to last
		"-3": runs[0], // oldest of the 4
	} {
		got, err := resolveRunDir(base, arg)
		require.NoError(t, err)
		assert.Equal(t, got, filepath.Join(base, want))
	}

	for _, arg := range []string{"-4", "5"} { // past the oldest; no such number
		_, err := resolveRunDir(base, arg)
		assert.Error(t, err, assert.AnyError)
	}
}

// --run=all scores every run under .pats/runs, not just the latest.
func TestScoreAllRuns(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.sh"), []byte("#!/bin/sh\necho 1.0\n"), 0o755))

	runs := []string{"001-20260701-fluffy-bunny", "002-20260702-undyed-purse"}
	for _, r := range runs {
		od := filepath.Join(dir, ".pats", "runs", r, "a", "t")
		require.NoError(t, os.MkdirAll(od, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))
	}

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
tasks: {kind: task, defaults: {prompt: p.txt}, items: {t: ~}}
scorers: {kind: scorer, items: {one: {score: ./one.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: [t], scorers: [one]}}}
`)

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: "all", Out: &out})
	require.NoError(t, err)
	assert.Equal(t, rep.Overall, 1.0) // the last run's report
	// no report tables under all -- just the per-cell log lines.
	assert.That(t, !strings.Contains(out.String(), "overall"), "no report table under all")
	assert.That(t, !strings.Contains(out.String(), "run stats"), "no stats table under all")

	for _, r := range runs { // every run got its scores.json
		_, err := os.Stat(filepath.Join(dir, ".pats", "runs", r, "scores.json"))
		require.NoError(t, err)
	}
}

// FuzzParseScore: scorer stdout isn't trusted input (arbitrary user script),
// just wants no panic.
func FuzzParseScore(f *testing.F) {
	for _, s := range []string{"0.5\n", "na\n", "", "\n\n1\n", "inf", "1e400", "-0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		parseScore(s)
	})
}
