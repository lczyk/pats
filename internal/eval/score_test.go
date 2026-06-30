package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
)

// score phase, no docker: an exec scorer reads each pair's stdout.log and emits
// a float; pats aggregates per (agent,task) then per agent (test-matrix weight).
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

	cfg := &config.Config{
		Sandboxes:    []config.Sandbox{{ID: "s", Kind: "bwrap"}},
		Agents:       []config.Agent{{ID: "a", Kind: "opencode-openrouter", Model: "m", Sandbox: "s"}},
		Tasks:        []config.Task{{ID: "t1", Prompt: "p.txt"}, {ID: "t2", Prompt: "p.txt"}},
		Scorers:      []config.Scorer{{ID: "has-good", Score: "has-good.sh"}},
		TestMatrix:   []config.Row{{Agent: config.StrList{"a"}, Task: config.StrList{"*"}}},
		ScorerMatrix: []config.Row{{Scorer: config.StrList{"has-good"}, Task: config.StrList{"*"}}},
	}

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)

	assert.Equal(t, rep.PerPair["a/t1"], 1.0)
	assert.Equal(t, rep.PerPair["a/t2"], 0.0)
	assert.Equal(t, rep.PerAgent["a"], 0.5) // equal task weights
	assert.Equal(t, rep.Overall, 0.5)

	// scores.json written into the run dir.
	_, err = os.Stat(filepath.Join(run, "scores.json"))
	require.NoError(t, err)
}

func TestScoreWeightedScorers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.sh"), []byte("#!/bin/sh\necho 1.0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zero.sh"), []byte("#!/bin/sh\necho 0.0\n"), 0o755))

	run := filepath.Join(dir, ".pats", "runs", "20260621-1")
	od := filepath.Join(run, "a", "t")
	require.NoError(t, os.MkdirAll(od, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(od, "stdout.log"), []byte("x"), 0o644))

	w3 := 3.0
	cfg := &config.Config{
		Sandboxes:  []config.Sandbox{{ID: "s", Kind: "bwrap"}},
		Agents:     []config.Agent{{ID: "a", Kind: "opencode-openrouter", Model: "m", Sandbox: "s"}},
		Tasks:      []config.Task{{ID: "t", Prompt: "p.txt"}},
		Scorers:    []config.Scorer{{ID: "one", Score: "one.sh"}, {ID: "zero", Score: "zero.sh"}},
		TestMatrix: []config.Row{{Agent: config.StrList{"a"}, Task: config.StrList{"t"}}},
		ScorerMatrix: []config.Row{
			{Scorer: config.StrList{"one"}, Task: config.StrList{"t"}, Weight: &w3}, // weight 3 -> 1.0
			{Scorer: config.StrList{"zero"}, Task: config.StrList{"t"}},             // weight 1 -> 0.0
		},
	}

	var out bytes.Buffer
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: run, Out: &out})
	require.NoError(t, err)
	// (3*1.0 + 1*0.0) / (3+1) = 0.75
	assert.Equal(t, rep.PerPair["a/t"], 0.75)
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
