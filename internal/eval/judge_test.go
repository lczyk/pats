package eval

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// a judge's reply in opencode's stream: one step, its text, its cost.
func opencodeReply(text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"step_start","part":{}}` + "\n" +
		`{"type":"text","part":{"text":` + string(b) + `}}` + "\n" +
		`{"type":"step_finish","part":{"cost":0.01,"tokens":{"input":1,"output":1}}}` + "\n"
}

const judgeCfg = `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: jm, sandbox: sxx}}}
tasks: {kind: task, defaults: {prompt: p}, items: {t1x: ~, t2x: ~, t3x: ~}}
scorers: {kind: scorer, items: {
  jud: {agent: jxx, prompt: grade it, stage: 'echo "$PATS_SCORED_AGENT_NAME $PATS_SCORER_NAME" > "$PATS_WORKDIR/staged"'},
  scr: {score: echo 1}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: [t1x, t2x, t3x], scorers: [jud, scr]}}}
`

// judgeFixture is a config dir with one run: t1x ran fine, t2x came home
// with nothing, t3x has no metadata. a stale verdict sits on t2x.
func judgeFixture(t *testing.T) (dir, run string, cfg *config.Config) {
	t.Helper()
	dir = t.TempDir()
	run = filepath.Join(dir, runsSubdir, "001-20260621-a-b")
	pair := func(task string, files map[string]string) {
		d := filepath.Join(run, "axx", task)
		require.NoError(t, os.MkdirAll(d, 0o755))
		for name, body := range files {
			require.NoError(t, os.WriteFile(filepath.Join(d, name), []byte(body), 0o644))
		}
	}
	pair("t1x", map[string]string{"metadata.json": `{"status":"ok","synced":true}`, "stdout.log": "did it", "result.txt": "r"})
	pair("t2x", map[string]string{"metadata.json": `{"status":"timeout","synced":false}`})
	pair("t3x", map[string]string{"stdout.log": "half"})
	stale := judgementDir(run, "jud", "axx", "t2x")
	require.NoError(t, os.MkdirAll(stale, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "verdict.json"), []byte(`{"ok":true,"score":1}`), 0o644))
	require.NoError(t, writeJSON(filepath.Join(run, "scores.json"), &ScoreReport{
		Version: scoresVersion, RunDir: run, Scores: []ScoreResult{
			{Agent: "axx", Task: "t1x", Scorer: "scr", Score: 1},
			{Agent: "axx", Task: "t2x", Scorer: "jud", Score: 1}, // stale: t2x is na now
		},
	}))
	cfg = parseCfg(t, judgeCfg)
	pinAll(t, dir, cfg)
	return dir, run, cfg
}

func useFake(t *testing.T, f *fakeSandbox) {
	t.Helper()
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return f, nil }
	t.Cleanup(func() { newSandbox = old })
}

func TestJudge(t *testing.T) {
	var workdirs []map[string]bool
	var prompts []string
	fake := &fakeSandbox{stdout: opencodeReply("looks right\nSCORE: 0.5"), onRun: func(spec sandbox.Spec) {
		seen := map[string]bool{}
		filepath.WalkDir(spec.Workdir, func(p string, d os.DirEntry, err error) error {
			if rel, _ := filepath.Rel(spec.Workdir, p); rel != "." {
				seen[rel] = true
			}
			return nil
		})
		workdirs = append(workdirs, seen)
		for _, m := range spec.Mounts {
			if m.Container == homeMount {
				b, _ := os.ReadFile(filepath.Join(m.Host, promptName))
				prompts = append(prompts, string(b))
			}
		}
	}}
	useFake(t, fake)
	dir, run, cfg := judgeFixture(t)

	var out bytes.Buffer
	err := Judge(cfg, JudgeOptions{ConfigDir: dir, Out: &out, Repeat: 2, Offline: true})
	require.NoError(t, err, "%s", out.String())
	assert.ContainsString(t, out.String(), "judging 1 scores (x 2 samples) with jxx")
	assert.ContainsString(t, out.String(), "1 judged (0 failed), 1 na, 1 skipped")

	// the judge saw the agent's output and what stage added, nothing pats wrote.
	require.Len(t, workdirs, 2)
	for _, w := range workdirs {
		assert.That(t, w["stdout.log"] && w["result.txt"] && w["staged"], "copy and stage: %v", w)
		assert.That(t, !w["metadata.json"], "metadata.json reached the judge")
	}
	require.Len(t, prompts, 2)
	assert.That(t, strings.HasPrefix(prompts[0], "grade it") && strings.Contains(prompts[0], "SCORE: na"), "prompt: %q", prompts[0])

	jd := judgementDir(run, "jud", "axx", "t1x")
	v, err := readVerdict(jd)
	require.NoError(t, err)
	assert.That(t, v.OK && *v.Score == 0.5, "verdict: %+v", v)
	assert.Equal(t, v.Samples, 2)
	assert.Equal(t, v.Judge, "jxx")
	for _, i := range []string{"1", "2"} {
		sv, err := readVerdict(filepath.Join(jd, i))
		require.NoError(t, err)
		assert.That(t, sv.OK, "sample %s", i)
		var m pairMeta
		b, err := os.ReadFile(filepath.Join(jd, i, "metadata.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &m))
		assert.Equal(t, m.Agent, "jxx")
		assert.That(t, m.Judged != nil && m.Judged.Task == "t1x", "judged: %+v", m.Judged)
	}
	for _, gone := range []string{jd + ".new", jd + ".old", judgementDir(run, "jud", "axx", "t2x")} {
		_, err := os.Stat(gone)
		assert.That(t, os.IsNotExist(err), gone+" left behind")
	}

	rep, err := readScores(run)
	require.NoError(t, err)
	got := map[string]float64{}
	for _, s := range rep.Scores {
		got[s.Agent+"/"+s.Task+"/"+s.Scorer] = s.Score
	}
	assert.Equal(t, got["axx/t1x/jud"], 0.5)
	assert.Equal(t, got["axx/t1x/scr"], 1.0) // a script score stays
	_, stale := got["axx/t2x/jud"]
	assert.That(t, !stale, "the stale na score stayed")
	assert.Equal(t, rep.FailedPairs["axx"], 1)

	// a plain score keeps what the judge made.
	rep2, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: io.Discard})
	require.NoError(t, err)
	assert.Equal(t, rep2.PerPair["axx/t1x"], 0.75)

	// the report counts the pairs no judge could grade, and the judges' work.
	doc := buildReport(run, rep2, io.Discard)
	require.Len(t, doc.Judges, 1)
	j := doc.Judges[0]
	assert.That(t, j.Name == "jxx" && j.Judgements == 1 && j.Samples == 2 && j.Failed == 0, "judges: %+v", j)
	assert.That(t, j.CostUSD != nil && *j.CostUSD > 0.019, "judge cost: %v", j.CostUSD)
	require.Len(t, doc.Agents, 1)
	assert.That(t, doc.Agents[0].FailedPairs != nil && *doc.Agents[0].FailedPairs == 1, "failed pairs")
	var human bytes.Buffer
	report(&human, run, rep2, false, io.Discard)
	assert.ContainsString(t, human.String(), "failed")
	assert.ContainsString(t, human.String(), "judge jxx: 1 judged, 0 failed, 0 na, 2 samples")
}

// a judge that does not end in a SCORE: line is a failed verdict, and its
// score leaves scores.json.
func TestJudgeFailedVerdict(t *testing.T) {
	useFake(t, &fakeSandbox{stdout: opencodeReply("I am not sure")})
	dir, run, cfg := judgeFixture(t)
	var out bytes.Buffer
	require.NoError(t, Judge(cfg, JudgeOptions{ConfigDir: dir, Out: &out, Offline: true}), "%s", out.String())
	v, err := readVerdict(judgementDir(run, "jud", "axx", "t1x"))
	require.NoError(t, err)
	assert.That(t, !v.OK, "a failed sample made a score")
	assert.ContainsString(t, v.Error, "no sample scored")
	assert.ContainsString(t, out.String(), "1 judged (1 failed)")
}

func TestJudgeSelectionErrors(t *testing.T) {
	useFake(t, &fakeSandbox{})
	dir, _, cfg := judgeFixture(t)
	err := Judge(cfg, JudgeOptions{ConfigDir: dir, Out: io.Discard, Offline: true, Scorers: []string{"scr"}})
	assert.Error(t, err, "is a script scorer")

	other := t.TempDir()
	err = Judge(cfg, JudgeOptions{ConfigDir: dir, Out: io.Discard, Offline: true, RunDir: other})
	assert.Error(t, err, "needs a run of this config")

	err = Judge(cfg, JudgeOptions{ConfigDir: dir, Out: io.Discard, Offline: true, Runs: 2})
	assert.Error(t, err, "asked for the latest 2 runs")
}

// a run that judges nothing (every pair na or skipped) makes no sandbox,
// and still drops the stale scores.
func TestJudgeNothingToJudge(t *testing.T) {
	fake := &fakeSandbox{}
	useFake(t, fake)
	dir, run, cfg := judgeFixture(t)
	var out bytes.Buffer
	require.NoError(t, Judge(cfg, JudgeOptions{ConfigDir: dir, Out: &out, Offline: true, Tasks: []string{"t2x", "t3x"}}), "%s", out.String())
	assert.Equal(t, fake.preflights, 0)
	assert.Equal(t, fake.calls, 0)
	rep, err := readScores(run)
	require.NoError(t, err)
	for _, s := range rep.Scores {
		assert.That(t, s.Scorer != "jud", "stale agent score kept: %+v", s)
	}
}

func TestRepairJudgement(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "t1x")
	mk := func(p string) { require.NoError(t, os.MkdirAll(p, 0o755)) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	// a crash between the renames: the old set comes back.
	mk(dir + ".old")
	mk(dir + ".new")
	require.NoError(t, repairJudgement(dir))
	assert.That(t, exists(dir) && !exists(dir+".old") && !exists(dir+".new"), "old set not restored")

	// a crash after the second rename: the new set stays, the old goes.
	mk(dir + ".old")
	require.NoError(t, repairJudgement(dir))
	assert.That(t, exists(dir) && !exists(dir+".old"), "old set not removed")
}
