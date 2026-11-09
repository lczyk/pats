package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestParseVerdict(t *testing.T) {
	for _, c := range []struct {
		text  string
		score float64
		na    bool
		ok    bool
	}{
		{"looks fine\nSCORE: 0.75", 0.75, false, true},
		{"SCORE: .5\n\n", 0.5, false, true},
		{"score: 1", 1, false, true},
		{"SCORE: 1.00", 1, false, true},
		{"SCORE: 0", 0, false, true},
		{"SCORE: na", 0, true, true},
		{"  SCORE:   NA  ", 0, true, true},
		{"SCORE: 1.5", 0, false, false},
		{"SCORE: 00.5", 0, false, false},
		{"SCORE: 2", 0, false, false},
		{"SCORE: -0", 0, false, false},
		{"SCORE: 0.5/1", 0, false, false},
		// an earlier SCORE: line is never read.
		{"SCORE: 1\nbut on reflection, no", 0, false, false},
		{"", 0, false, false},
	} {
		score, na, ok := parseVerdict(c.text)
		assert.Equal(t, ok, c.ok, c.text)
		assert.Equal(t, na, c.na, c.text)
		assert.Equal(t, score, c.score, c.text)
	}
}

func writeLog(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdout.log")
	require.NoError(t, os.WriteFile(p, []byte(s), 0o644))
	return p
}

func TestJudgeVerdictPerHarness(t *testing.T) {
	for _, c := range []struct {
		name, harness, log string
		score              float64
		na                 bool
		err                string
	}{
		{"claude", "claude-cli-keyless",
			`{"type":"user","message":{"content":"SCORE: 1"}}` + "\n" +
				`{"type":"result","result":"fine\nSCORE: 0.5"}` + "\n", 0.5, false, ""},
		{"claude, a later note", "claude-cli-keyless",
			`{"type":"result","result":"SCORE: 0.25"}` + "\n" +
				`{"type":"result","result":"verdict stands"}` + "\n", 0.25, false, ""},
		{"claude, a botched later score", "claude-cli-keyless",
			`{"type":"result","result":"SCORE: 0.25"}` + "\n" +
				`{"type":"result","result":"SCORE: 0.9 actually\nok"}` + "\n", 0, false, "does not end in a SCORE"},
		{"codex", "codex-cli-keyless",
			`{"type":"item.completed","item":{"type":"agent_message","text":"SCORE: 0.1"}}` + "\n" +
				`{"type":"item.completed","item":{"type":"command_execution"}}` + "\n" +
				`{"type":"item.completed","item":{"type":"agent_message","text":"done\nSCORE: na"}}` + "\n", 0, true, ""},
		{"copilot", "copilot-cli-keyless",
			`{"type":"assistant.message","data":{"content":"SCORE: 0.2"}}` + "\n" +
				`{"type":"assistant.message","data":{"content":"graded\nSCORE: 0.8"}}` + "\n" +
				`{"type":"assistant.message","data":{"content":""}}` + "\n", 0.8, false, ""},
		{"opencode", "opencode-openrouter",
			`{"type":"step_start","part":{}}` + "\n" +
				`{"type":"text","part":{"text":"SCORE: 0.3"}}` + "\n" +
				`{"type":"step_finish","part":{}}` + "\n" +
				`{"type":"text","part":{"text":"graded"}}` + "\n" +
				`{"type":"text","part":{"text":"SCORE: 0.6"}}` + "\n" +
				`{"type":"step_finish","part":{}}` + "\n", 0.6, false, ""},
		{"no reply", "codex-cli-keyless", `{"type":"turn.failed"}` + "\n", 0, false, "no final reply"},
		// a SCORE: line in a tool result is not the reply.
		{"no score", "claude-cli-keyless", `{"type":"result","result":"I could not decide"}` + "\n", 0, false, "does not end in a SCORE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			score, na, err := judgeVerdict(c.harness, writeLog(t, c.log))
			if c.err != "" {
				assert.Error(t, err, c.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, score, c.score)
			assert.Equal(t, na, c.na)
		})
	}
}

// the judge's copy holds what the agent made: no pats logs, no links, nothing
// from outside the run.
func TestCopyJudged(t *testing.T) {
	runDir := t.TempDir()
	pair := filepath.Join(runDir, "axx", "txx")
	require.NoError(t, os.MkdirAll(filepath.Join(pair, "out"), 0o755))
	for name, body := range map[string]string{
		"stdout.log": "s", "metadata.json": "{}", "stderr.log": "e", "egress.log": "g",
		"collect.log": "c", "result.txt": "r", "out/deep.txt": "d",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(pair, name), []byte(body), 0o644))
	}
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("x"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(pair, "link")))
	require.NoError(t, os.Symlink(filepath.Join(pair, "out"), filepath.Join(pair, "dirlink")))

	dst := t.TempDir()
	skipped, err := copyJudged(runDir, pair, dst)
	require.NoError(t, err)
	assert.Equal(t, skipped, 2)
	for _, name := range []string{"stdout.log", "result.txt", "out/deep.txt"} {
		_, err := os.Stat(filepath.Join(dst, name))
		assert.NoError(t, err)
	}
	for _, name := range []string{"metadata.json", "stderr.log", "egress.log", "collect.log", "link", "dirlink"} {
		_, err := os.Lstat(filepath.Join(dst, name))
		assert.That(t, os.IsNotExist(err), name+" was copied")
	}

	// a pair dir that resolves outside the run is refused.
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(runDir, "axx", "evil")))
	_, err = copyJudged(runDir, filepath.Join(runDir, "axx", "evil"), t.TempDir())
	assert.Error(t, err, "not inside the run dir")
}

func TestCombine(t *testing.T) {
	f := func(x float64) *float64 { return &x }
	ok := func(x float64) verdict { return verdict{OK: true, Score: f(x)} }
	na := verdict{OK: true, NA: true}
	bad := verdict{Error: "x"}

	v := combine(3, []verdict{ok(0.5), ok(1), bad}, verdict{})
	assert.That(t, v.OK && *v.Score == 0.75, "mean of the numeric samples")
	assert.Equal(t, v.Failed, 1)
	assert.Equal(t, v.Numeric, 2)
	assert.That(t, v.SD != nil, "sd with two samples")

	v = combine(2, []verdict{ok(0.5), na}, verdict{})
	assert.That(t, v.OK && *v.Score == 0.5 && v.SD == nil, "na left out of the mean")

	v = combine(2, []verdict{na, na}, verdict{})
	assert.That(t, v.OK && v.NA, "all na is na")

	v = combine(2, []verdict{na, bad}, verdict{})
	assert.That(t, !v.OK, "na with a failure is a failure")

	v = combine(3, nil, verdict{})
	assert.That(t, !v.OK && v.Ran == 0 && v.Samples == 3, "none ran")
}

func TestJudgeRowOf(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, judgeRowOf(dir), rowSkipped)
	for meta, want := range map[string]judgeRow{
		`{"status":"ok","synced":true}`:                             rowJudge,
		`{"status":"error","synced":true}`:                          rowJudge,
		`{"status":"timeout","synced":false}`:                       rowNA,
		`{"status":"ok","synced":true,"summary":{"is_error":true}}`: rowNA,
		`{"status":"nonzero"}`:                                      rowJudge,
		`{"status":"timeout"}`:                                      rowNA,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o644))
		assert.Equal(t, judgeRowOf(dir), want, meta)
	}
}
