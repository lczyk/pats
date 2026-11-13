package eval

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// FuzzParseVerdict: a judge's reply is text a model wrote. what is read from
// it is na or a number in [0,1], never one to clamp, and a reply that is not
// a score reads as the zero verdict; only the last non-empty line counts, so
// nothing before it and no blank space after it changes the reading; and the
// plain decimal of every number from 0 to 1 is a score, of any other (-0
// too) is not.
func FuzzParseVerdict(f *testing.F) {
	for _, s := range []string{
		"looks fine\nSCORE: 0.75", "SCORE: .5\n\n", "score: 1", "SCORE: 1.00", "SCORE: na", "  SCORE:   NA  ",
		"SCORE: 1.5", "SCORE: 00.5", "SCORE: -0", "SCORE: 0.5/1", "SCORE: 1\nbut on reflection, no", "",
		"SCORE: 0.99999999999999999999", "SCORE: 1e-3", "SCORE: nan", "SCORE: 1\r\n", "SCORE: 1\n\f", "\xc5\xbfCORE: 1",
	} {
		f.Add("", s, 0.5)
		f.Add("SCORE: 1\n", s, 1.5)
	}
	f.Fuzz(func(t *testing.T, before, text string, x float64) {
		score, na, ok := parseVerdict(text)
		assert.That(t, score >= 0 && score <= 1, "score %v out of [0,1] from %q", score, text)
		if !ok || na {
			assert.Equal(t, score, 0.0, "a number beside na or a failure: %q", text)
		}
		assert.That(t, ok || !na, "na from a reply that did not parse: %q", text)

		same := func(v string) {
			t.Helper()
			s2, na2, ok2 := parseVerdict(v)
			assert.That(t, s2 == score && na2 == na && ok2 == ok,
				"%q reads (%v, %v, %v), %q reads (%v, %v, %v)", text, score, na, ok, v, s2, na2, ok2)
		}
		same(text + " \t\r\n\n")
		if strings.TrimRight(text, " \t\r\n") != "" {
			same(before + "\n" + text)
		}
		if ok && !na {
			same("SCORE: " + strconv.FormatFloat(score, 'f', -1, 64))
		}

		got, gotNA, gotOK := parseVerdict("SCORE: " + strconv.FormatFloat(x, 'f', -1, 64))
		if x >= 0 && x <= 1 && !math.Signbit(x) {
			assert.That(t, gotOK && !gotNA && got == x, "%v in [0,1] reads (%v, %v, %v)", x, got, gotNA, gotOK)
		} else {
			assert.That(t, !gotOK, "%v is no score, yet reads (%v, %v, %v)", x, got, gotNA, gotOK)
		}
	})
}

// FuzzFinalTexts: a judge's stream is its harness's ndjson, with model text
// and tool output inside. whatever the bytes, every harness has a reader and
// it ends; lines that are not json change nothing; only claude keeps more
// than one reply; and the verdict is parseVerdict's reading of the newest
// reply that mentions SCORE: at all, in range, or an error.
func FuzzFinalTexts(f *testing.F) {
	for _, s := range []string{
		`{"type":"user","message":{"content":"SCORE: 1"}}` + "\n" + `{"type":"result","result":"fine\nSCORE: 0.5"}` + "\n",
		`{"type":"result","result":"SCORE: 0.25"}` + "\n" + `{"type":"result","result":"verdict stands"}` + "\n",
		`{"type":"result","result":"SCORE: 0.25"}` + "\n" + `{"type":"result","result":"SCORE: 0.9 actually\nok"}` + "\n",
		`{"type":"item.completed","item":{"type":"agent_message","text":"SCORE: 0.1"}}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"done\nSCORE: na"}}` + "\n",
		`{"type":"assistant.message","data":{"content":"graded\nSCORE: 0.8"}}` + "\n" +
			`{"type":"assistant.message","data":{"content":" "}}` + "\n",
		`{"type":"step_start","part":{}}` + "\n" + `{"type":"text","part":{"text":"SCORE: 0.3"}}` + "\n" +
			`{"type":"step_finish","part":{}}` + "\n" + `{"type":"text","part":{"text":"graded"}}` + "\n" +
			`{"type":"text","part":{"text":"SCORE: 0.6"}}` + "\n" + `{"type":"step_finish","part":{}}` + "\n",
		`{"type":"turn.failed"}` + "\n", "", "not json\n", "null\n[]\n1\n", `{"type":"result","result":7}` + "\n",
		`{"type":"result","result":"SCORE: 1"}`, "\r\n" + `{"type":"text","part":{"text":"SCORE: 1"}}` + "\r\n",
	} {
		f.Add([]byte(s))
	}
	// one dir for every input: a dir each would be most of the run.
	dir := f.TempDir()
	path, noisy := filepath.Join(dir, "stdout.log"), filepath.Join(dir, "noisy.log")
	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(path, data, 0o644))
		require.NoError(t, os.WriteFile(noisy, slices.Concat([]byte("not json\n"), data, []byte("\n{\"type\":\n")), 0o644))

		for _, h := range harnessNames() {
			texts, err := finalTexts(hd(h), path)
			if err != nil {
				// only a line past the scanner's cap stops a read.
				assert.ErrorIs(t, err, bufio.ErrTooLong, h)
				continue
			}
			if h != "claude-cli-keyless" {
				assert.That(t, len(texts) <= 1, "%s kept %d replies", h, len(texts))
			}
			again, err := finalTexts(hd(h), noisy)
			require.NoError(t, err, h)
			assert.EqualArrays(t, again, texts, h)

			// newest first: the first reply that mentions SCORE: decides.
			want, wantNA, wantOK := 0.0, false, false
			for _, reply := range texts {
				if scoreMention.MatchString(reply) {
					want, wantNA, wantOK = parseVerdict(reply)
					break
				}
			}
			score, na, err := judgeVerdict(hd(h), path)
			if len(texts) == 0 {
				assert.ErrorIs(t, err, errNoReply, h)
			}
			if !wantOK {
				assert.That(t, err != nil, "%s: a verdict (%v, %v) from no score line", h, score, na)
				continue
			}
			require.NoError(t, err, h)
			assert.That(t, score == want && na == wantNA, "%s: verdict (%v, %v), its reply reads (%v, %v)", h, score, na, want, wantNA)
			assert.That(t, score >= 0 && score <= 1, "%s: score %v out of [0,1]", h, score)
		}

		// a harness with no reader never yields a verdict.
		_, _, err := judgeVerdict(hd("no-such-harness"), path)
		assert.That(t, err != nil, "a verdict for an unknown harness")
	})
}

// COVER: a line past the stream scanner's 8 MiB cap ends the read with an
// error for every harness, rather than a hang or a verdict from the lines
// that did fit.
func TestFinalTextsLongLine(t *testing.T) {
	long := `{"type":"result","result":"` + strings.Repeat("x", 8<<20) + `\nSCORE: 1"}`
	p := writeLog(t, `{"type":"result","result":"SCORE: 1"}`+"\n"+long+"\n")
	for _, h := range harnessNames() {
		_, err := finalTexts(hd(h), p)
		assert.ErrorIs(t, err, bufio.ErrTooLong, h)
		_, _, err = judgeVerdict(hd(h), p)
		assert.ErrorIs(t, err, bufio.ErrTooLong, h)
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
			score, na, err := judgeVerdict(hd(c.harness), writeLog(t, c.log))
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

// a harness with no parse script has its whole stdout for a reply; no harness
// at all has none.
func TestFinalTextsNoParser(t *testing.T) {
	bare := tables(t, `harnesses: {bare: {run: "true"}}`).Harness("bare")
	texts, err := finalTexts(bare, writeLog(t, "graded\nSCORE: 0.4\n"))
	require.NoError(t, err)
	require.Len(t, texts, 1)
	score, na, err := judgeVerdict(bare, writeLog(t, "graded\nSCORE: 0.4\n"))
	require.NoError(t, err)
	assert.Equal(t, score, 0.4)
	assert.Equal(t, na, false)

	texts, err = finalTexts(bare, writeLog(t, " \n"))
	require.NoError(t, err)
	assert.Len(t, texts, 0)

	_, err = finalTexts(nil, writeLog(t, "SCORE: 1\n"))
	assert.That(t, err != nil, "a reply with no harness to read it by")
}

// a judge's sample with a line its parse script failed on has an error for a
// verdict, whatever reply was read before the line.
func TestSampleVerdictParseErrors(t *testing.T) {
	strict := tables(t, `
harnesses:
  strict:
    run: "true"
    parse: |
      ev = json.decode(line)
      out.usage(cost_usd = ev["cost"])
      out.final(ev["reply"])
`).Harness("strict")
	write := func(log string) string {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "stdout.log"), []byte(log), 0o644))
		m := pairMeta{Status: "ok", Summary: summarize(strict, filepath.Join(dir, "stdout.log"))}
		require.NoError(t, writeJSON(filepath.Join(dir, "metadata.json"), m))
		return dir
	}
	good := `{"cost": 1, "reply": "SCORE: 0.5"}` + "\n"
	v := sampleVerdict(write(good), strict, verdict{})
	assert.Equal(t, v.OK, true)

	// the second line would have taken the score back, and could not be read.
	v = sampleVerdict(write(good+`{"reply": "no, SCORE: 0"}`+"\n"), strict, verdict{})
	assert.Equal(t, v.OK, false)
	assert.ContainsString(t, v.Error, "1 line(s) failed to parse")
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
