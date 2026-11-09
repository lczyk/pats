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

// scoredRun lays out a scored run: a claude pair with a full summary, a codex
// pair whose summary has no turns or cost, an agent whose only pair failed
// unscored, and a pair whose metadata.json is half written.
func scoredRun(t *testing.T) (configDir, runDir string) {
	t.Helper()
	// glob metacharacters in the path: the pair dirs are listed, not globbed.
	configDir = filepath.Join(t.TempDir(), "proj[1]")
	runDir = filepath.Join(configDir, runsSubdir, "003-20260621-fluffy-bunny")
	meta := func(agent, task, body string) {
		d := filepath.Join(runDir, agent, task)
		require.NoError(t, os.MkdirAll(d, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "metadata.json"), []byte(body), 0o644))
	}
	meta("claude", "t1", `{"agent":"claude","harness":"claude-cli-keyless","task":"t1","status":"ok","duration_s":12.5,
		"denied_egress":["evil.example"],
		"summary":{"num_turns":4,"has_cost":true,"cost_usd":0.25,"input_tokens":10,"output_tokens":20,
		"cache_read_tokens":300,"tools":{"Bash":3}}}`)
	meta("codex", "t1", `{"agent":"codex","harness":"codex-cli-keyless","task":"t1","status":"nonzero","duration_s":3,
		"error":"exit 1","summary":{"has_cost":false,"input_tokens":7,"output_tokens":9,"reasoning_tokens":2}}`)
	meta("broken", "t1", `{"agent":"broken","task":"t1","status":"error","duration_s":1}`)
	// a dot-prefixed agent id is a valid one; its pair is read like any other.
	meta(".base", "t1", `{"agent":".base","task":"t1","status":"ok","duration_s":2}`)
	meta("claude", "t2", `{"agent":"claude","sta`)
	rep := aggregate(runDir, []ScoreResult{
		{Agent: "codex", Task: "t1", Scorer: "s2", Score: 0.5},
		{Agent: "claude", Task: "t1", Scorer: "s2", Score: 1},
		{Agent: "claude", Task: "t1", Scorer: "s1", Score: 0.5},
		{Agent: "gone", Task: "t1", Scorer: "s1", Score: 1}, // scored, its metadata.json since lost
	}, nil)
	// aggregate's per-agent means come from the suites' pairs; set them as a
	// score over every pair would.
	rep.PerAgent = map[string]float64{"claude": 0.75, "codex": 0.5, "gone": 1}
	rep.Overall = 0.75
	require.NoError(t, writeJSON(filepath.Join(runDir, "scores.json"), rep))
	return configDir, runDir
}

func TestReportJSON(t *testing.T) {
	configDir, runDir := scoredRun(t)
	var out, errOut bytes.Buffer
	require.NoError(t, Report(configDir, "", 0, true, &out, &errOut))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc), "stdout is one json document")
	assert.Equal(t, doc["version"], any(float64(1)))
	run := doc["run"].(map[string]any)
	assert.Equal(t, run["name"], any("003-20260621-fluffy-bunny"))
	assert.That(t, filepath.IsAbs(run["dir"].(string)), "run.dir is absolute")
	assert.Equal(t, doc["overall"], any(0.75))

	// every agent with a pair, the failed one with a null mean.
	agents := doc["agents"].([]any)
	assert.Len(t, agents, 5)
	assert.Equal(t, agents[0].(map[string]any)["name"], any(".base"))
	assert.Equal(t, agents[1].(map[string]any)["name"], any("broken"))
	assert.Equal(t, agents[1].(map[string]any)["mean"], any(nil))
	assert.Equal(t, agents[2].(map[string]any)["mean"], any(0.75))

	// pairs sorted by agent then task; unknown is null, never zero.
	pairs := doc["pairs"].([]any)
	assert.Len(t, pairs, 6)
	assert.Equal(t, pairs[0].(map[string]any)["status"], any("ok")) // .base/t1
	pairs = pairs[1:]
	p := func(i int) map[string]any { return pairs[i].(map[string]any) }
	assert.Equal(t, p(0)["agent"], any("broken"))
	assert.Equal(t, p(0)["mean"], any(nil))
	assert.Equal(t, p(0)["turns"], any(nil))
	assert.Equal(t, p(1)["task"], any("t1"))
	assert.Equal(t, p(1)["mean"], any(0.75))
	assert.Equal(t, p(1)["turns"], any(float64(4)))
	assert.Equal(t, p(1)["cost_usd"], any(0.25))
	assert.Equal(t, p(1)["cache_creation_tokens"], any(nil))
	assert.Equal(t, p(1)["error"], any(nil))
	assert.EqualArrays(t, p(1)["denied_egress"].([]any), []any{"evil.example"})
	assert.EqualArrays(t, p(1)["blocked_egress"].([]any), []any{}) // known empty: [], not null
	// a half-written metadata.json: listed, run fields null, warned about.
	assert.Equal(t, p(2)["task"], any("t2"))
	assert.Equal(t, p(2)["status"], any(nil))
	assert.ContainsString(t, errOut.String(), filepath.Join("claude", "t2", "metadata.json"))
	// codex: no turns, no dollar cost.
	assert.Equal(t, p(3)["turns"], any(nil))
	assert.Equal(t, p(3)["cost_usd"], any(nil))
	assert.Equal(t, p(3)["input_tokens"], any(float64(7)))
	assert.Equal(t, p(3)["error"], any("exit 1"))
	// a scored pair with no metadata.json: listed, with its mean, status null.
	assert.Equal(t, p(4)["agent"], any("gone"))
	assert.Equal(t, p(4)["mean"], any(float64(1)))
	assert.Equal(t, p(4)["status"], any(nil))

	// scores sorted by agent, task, scorer.
	scores := doc["scores"].([]any)
	assert.Len(t, scores, 4)
	assert.Equal(t, scores[0].(map[string]any)["scorer"], any("s1"))
	assert.Equal(t, scores[2].(map[string]any)["agent"], any("codex"))

	// the same run reports the same bytes.
	var again bytes.Buffer
	require.NoError(t, Report(configDir, "", 0, true, &again, &bytes.Buffer{}))
	assert.Equal(t, again.String(), out.String())
	_ = runDir
}

func TestReportErrors(t *testing.T) {
	configDir, runDir := scoredRun(t)

	// another pats's scores.json: an error naming the rescore, nothing on stdout.
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "scores.json"), []byte(`{"cells":[],"overall":1}`), 0o644))
	var out bytes.Buffer
	err := Report(configDir, "", 0, true, &out, &bytes.Buffer{})
	assert.ErrorIs(t, err, errOldScores)
	assert.ContainsString(t, err.Error(), "pats score -r 003-20260621-fluffy-bunny")
	assert.Equal(t, out.Len(), 0)

	require.NoError(t, os.Remove(filepath.Join(runDir, "scores.json")))
	err = Report(configDir, "", 0, true, &out, &bytes.Buffer{})
	assert.Error(t, err, "no scores for")
	assert.Equal(t, out.Len(), 0)
}

func TestReportPivot(t *testing.T) {
	_, runDir := scoredRun(t)
	rep, err := readScores(runDir)
	require.NoError(t, err)
	var buf, warn bytes.Buffer
	report(&buf, runDir, rep, false, &warn)
	out := buf.String()
	assert.ContainsString(t, warn.String(), filepath.Join("claude", "t2", "metadata.json"))

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	row := func(label string) string {
		for _, l := range lines {
			if f := strings.Fields(l); len(f) > 0 && f[0] == label {
				return l
			}
		}
		t.Fatalf("no %s row in:\n%s", label, out)
		return ""
	}
	// agents are the columns, every agent with a pair, sorted.
	assert.EqualArrays(t, strings.Fields(lines[0]), []string{".base", "broken", "claude", "codex", "gone"})
	// worst task first: t1 has codex's 0.5, t2 nothing scored.
	assert.ContainsString(t, lines[1], "t1")
	assert.ContainsString(t, lines[2], "t2")
	// a pair that did not end ok carries its mark; a scored pair with no
	// readable metadata is "?"; an unscored pair is "-".
	assert.EqualArrays(t, strings.Fields(row("t1")),
		[]string{"t1", "-", "-", "E", "0.75", "[#####--]", "0.50", "[####---]", "!", "1.00", "[#######]", "?"})
	assert.EqualArrays(t, strings.Fields(row("t2")), []string{"t2", "-", "-", "-", "?", "-", "-"})
	assert.EqualArrays(t, strings.Fields(row("avg")),
		[]string{"avg", "-", "-", "0.75", "[#####--]", "0.50", "[####---]", "1.00", "[#######]"})
	// cost sums known dollar costs; codex reports none.
	assert.EqualArrays(t, strings.Fields(row("cost")), []string{"cost", "-", "-", "$0.25", "-", "-"})
	assert.EqualArrays(t, strings.Fields(row("time")), []string{"time", "0:02", "0:01", "0:13", "0:03", "-"})
	assert.ContainsString(t, row("overall"), "0.75 [#####--]")
	// the legend names only the marks shown.
	assert.Equal(t, lines[len(lines)-1], "! nonzero  E error  ? no metadata")
	for _, gone := range []string{"breakdown", "run stats"} {
		assert.That(t, !strings.Contains(out, gone), "no %s section", gone)
	}

	assert.That(t, !strings.Contains(out, "\033["), "no ansi in plain output")
	buf.Reset()
	report(&buf, runDir, rep, true, &bytes.Buffer{})
	assert.ContainsString(t, buf.String(), "\033[32m")
	assert.ContainsString(t, buf.String(), "\033[33m")
}

// one task: no avg or overall rows, and with every pair ok no legend.
func TestReportOneTask(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	d := filepath.Join(runDir, "a", "t")
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "metadata.json"),
		[]byte(`{"agent":"a","task":"t","status":"ok","duration_s":3725}`), 0o644))
	rep := aggregate(runDir, []ScoreResult{{Agent: "a", Task: "t", Scorer: "s", Score: 1}},
		[]config.TestPair{{Agent: "a", Task: "t"}})
	var buf bytes.Buffer
	report(&buf, runDir, rep, false, nil)
	out := buf.String()
	assert.That(t, !strings.Contains(out, "avg"), "no avg row")
	assert.That(t, !strings.Contains(out, "overall"), "no overall row")
	assert.That(t, !strings.Contains(out, "nonzero"), "no legend")
	assert.ContainsString(t, out, "1:02:05") // over an hour
}

func TestScoreBar(t *testing.T) {
	assert.Equal(t, scoreBar(1), "[#######]")
	assert.Equal(t, scoreBar(0.94), "[######-]") // only a 1 fills it
	assert.Equal(t, scoreBar(0.5), "[####---]")
	assert.Equal(t, scoreBar(0), "[-------]")
}

// a pair dir that cannot be listed is warned about, not dropped silently.
func TestReadPairsUnlistable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any dir")
	}
	_, runDir := scoredRun(t)
	locked := filepath.Join(runDir, "codex")
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	var warn bytes.Buffer
	pairs := readPairs(runDir, nil, &warn)
	assert.ContainsString(t, warn.String(), "codex")
	for _, p := range pairs {
		assert.That(t, p.agent != "codex", "an unlistable dir yields no pairs")
	}
}

func TestWriteJSONReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	require.NoError(t, writeJSON(p, map[string]int{"a": 1}))
	require.NoError(t, writeJSON(p, map[string]int{"a": 2}))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, string(b), "{\n  \"a\": 2\n}\n")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1) // no temp file left behind
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o644))
}

// a log line past the scanner's cap stops the scan: no summary, rather than
// counts over part of the stream.
func TestSummaryScanCutShort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stdout.log")
	big := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 9<<20) + `"}]}}`
	log := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}` + "\n" + big + "\n"
	require.NoError(t, os.WriteFile(p, []byte(log), 0o644))
	assert.That(t, claudeSummary(p) == nil, "a scan cut short gives no summary")
}
