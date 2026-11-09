package eval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// multiRun writes a run dir with scores and metadata: scores maps
// agent/task to its scorer's score; meta maps agent/task to a status, ""
// for a pair dir with no metadata.
func multiRun(t *testing.T, base, name string, scores map[string]float64, meta map[string]string, cost float64) string {
	dir := filepath.Join(base, name)
	rep := &ScoreReport{Version: scoresVersion, RunDir: dir, PerPair: map[string]float64{}, PerAgent: map[string]float64{}}
	for k, s := range scores {
		a, tk := splitKey(k)
		rep.Scores = append(rep.Scores, ScoreResult{Agent: a, Task: tk, Scorer: "s", Score: s})
		rep.PerPair[k] = s
	}
	for k, st := range meta {
		a, tk := splitKey(k)
		pd := filepath.Join(dir, a, tk)
		require.NoError(t, os.MkdirAll(pd, 0o755))
		if st == "" {
			continue
		}
		m := pairMeta{Agent: a, Task: tk, Status: st, DurationS: 60, Summary: &runSummary{CostUSD: cost, HasCost: true}}
		b, _ := json.Marshal(m)
		require.NoError(t, os.WriteFile(filepath.Join(pd, "metadata.json"), b, 0o644))
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, writeJSON(filepath.Join(dir, "scores.json"), rep))
	return dir
}

func splitKey(k string) (string, string) {
	for i := range k {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// over several runs a pair's score is the mean of its scored attempts, with
// sd (n - 1) and n; an attempt with no score keeps its facts in samples but
// not in n; a pair dir with no metadata and no score is still an attempt;
// agents weigh pairs equally; cost sums every attempt.
func TestReportMulti(t *testing.T) {
	cfgDir := t.TempDir()
	base := filepath.Join(cfgDir, runsSubdir)
	multiRun(t, base, "001-20261101-a-b", map[string]float64{"a/t": 1, "b/t": 0.2}, map[string]string{"a/t": "ok", "b/t": "ok", "a/u": "ok"}, 1)
	multiRun(t, base, "002-20261102-c-d", map[string]float64{"a/t": 0.5}, map[string]string{"a/t": "ok", "b/t": "error", "a/u": ""}, 2)

	var out, errOut bytes.Buffer
	require.NoError(t, Report(cfgDir, "", 2, true, &out, &errOut))
	var doc multiDoc
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	assert.Equal(t, doc.Version, 2)
	assert.Len(t, doc.Runs, 2)
	pairs := map[string]multiPair{}
	for _, p := range doc.Pairs {
		pairs[p.Agent+"/"+p.Task] = p
	}
	at := pairs["a/t"]
	assert.Equal(t, at.N, 2)
	assert.Equal(t, *at.Mean, 0.75)
	assert.That(t, *at.SD > 0.35 && *at.SD < 0.36, "sd %v", *at.SD)
	bt := pairs["b/t"]
	assert.Equal(t, bt.N, 1)
	assert.That(t, bt.SD == nil, "sd at n 1")
	assert.Len(t, bt.Samples, 2) // the unscored error attempt is kept
	assert.Equal(t, *bt.Samples[1].Status, "error")
	au := pairs["a/u"]
	assert.Equal(t, au.N, 0)
	assert.Len(t, au.Samples, 2) // the second, a bare pair dir, too
	assert.That(t, au.Samples[1].Status == nil, "no metadata, no status")
	// agent a: mean of its pairs' means (a/u has none): 0.75; b: 0.2.
	assert.Equal(t, *doc.Overall, (0.75+0.2)/2)

	out.Reset()
	require.NoError(t, Report(cfgDir, "", 2, false, &out, &errOut))
	assert.ContainsString(t, out.String(), "0.75 +-0.35")
	assert.ContainsString(t, out.String(), "0.20 (1) E 1/2") // fewer scored runs, and the worst status with its count
	assert.ContainsString(t, out.String(), "$4.00")          // a: 1+2 for a/t, 1 for a/u's first attempt
}

// every run reported must be scored by this pats; more runs than there are
// is an error; one run is the plain report.
func TestReportMultiErrors(t *testing.T) {
	cfgDir := t.TempDir()
	base := filepath.Join(cfgDir, runsSubdir)
	multiRun(t, base, "001-20261101-a-b", map[string]float64{"a/t": 1}, map[string]string{"a/t": "ok"}, 1)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "002-20261102-c-d", "a", "t"), 0o755))
	var out bytes.Buffer
	err := Report(cfgDir, "", 2, true, &out, &out)
	assert.Error(t, err, "run 002-20261102-c-d is not scored")
	err = Report(cfgDir, "", 3, true, &out, &out)
	assert.Error(t, err, "asked for the latest 3 runs, there are 2")
}

// score -N takes the latest n runs up front: more than there are is an error.
func TestScoreLatestTooMany(t *testing.T) {
	cfgDir := t.TempDir()
	for _, n := range []string{"001-20261101-a-b", "002-20261102-c-d", "003-20261103-e-f"} {
		require.NoError(t, os.MkdirAll(filepath.Join(cfgDir, runsSubdir, n), 0o755))
	}
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	var out bytes.Buffer
	_, err := Score(cfg, ScoreOptions{ConfigDir: cfgDir, Runs: 4, Out: &out})
	assert.Error(t, err, "asked for the latest 4 runs, there are 3")
}
