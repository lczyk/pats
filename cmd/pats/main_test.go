package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestWantsVersion(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-v"}, true},
		{[]string{"version"}, true},
		{[]string{"-v", "run"}, false}, // verbosity, then the command
		{[]string{"-vv", "init"}, false},
		{[]string{"score", "-r", "-v"}, false}, // only the first arg counts
		{[]string{"run"}, false},
		{[]string{}, false},
	}
	for _, tc := range cases {
		got := wantsVersion(tc.args)
		assert.Equal(t, got, tc.want, tc.args)
	}
}

func TestColorize(t *testing.T) {
	in := "Usage:\n  pats [OPTIONS] COMMAND\n\nAvailable commands:\n  run    run the test-matrix\n\n[run command options]\n      -c, --config= path to pats.yaml\n"
	got := colorize(in)
	for _, want := range []string{
		"\x1b[1mUsage:\x1b[0m",
		"\x1b[1mAvailable commands:\x1b[0m",
		"  \x1b[36mrun\x1b[0m    run the test-matrix",
		"      \x1b[36m-c, --config=\x1b[0m path to pats.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("colorize output missing %q:\n%s", want, got)
		}
	}
}

// a flag wins over its env: an overridden env value is not even parsed, so a
// bad one cannot stop a run whose flag is fine.
func TestEgressRunFlagShadowsBadEnv(t *testing.T) {
	t.Setenv("PATS_EGRESS_POLICY", "default: [broken")
	t.Setenv("PATS_EGRESS_REFUSE", "not-a-cidr")
	dir := t.TempDir()
	pol := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(pol, []byte("allow: [a.test]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &EgressRunCommand{Policy: pol, Refuse: []string{"10.0.0.0/8"}, Listen: "bad address"}
	err := c.Execute(nil)
	// it gets past both env values and fails only at the (deliberately bad) listen.
	if err == nil || !strings.Contains(err.Error(), "bad address") {
		t.Fatalf("want a listen error, got %v", err)
	}
}

// --json is a preview flag: alone it never starts a run.
func TestRunJSONNeedsDryRun(t *testing.T) {
	err := (&RunCommand{JSON: true}).Execute(nil)
	assert.Error(t, err, "--json goes with -n/--dry-run only")
}

// a repeat count below one is an error before anything runs.
func TestRunRepeatAtLeastOne(t *testing.T) {
	for _, n := range []int{0, -1} {
		err := (&RunCommand{Repeat: n}).Execute(nil)
		assert.Error(t, err, "--repeat must be at least 1")
	}
}

// a cap is a positive finite number of dollars.
func TestRunMaxCost(t *testing.T) {
	for _, c := range []float64{-1, math.Inf(1), math.NaN(), 2e6} {
		err := (&RunCommand{Repeat: 1, MaxCost: c}).Execute(nil)
		assert.Error(t, err, "--max-cost")
	}
}

// an explicit zero is told from an absent flag in any spelling the parser
// takes, grouped short flags included, before anything runs.
func TestRunsZeroGrouped(t *testing.T) {
	for _, args := range [][]string{{"score", "-vN", "0"}, {"report", "-N0"}, {"report", "--runs=0"}, {"run", "--max-cost", "0"}} {
		opts = Options{}
		parser = newParser()
		_, err := parser.ParseArgs(args)
		require.That(t, err != nil, "%v: no error", args)
		assert.That(t, strings.Contains(err.Error(), "--runs must be at least 1") || strings.Contains(err.Error(), "--max-cost"), "%v: %v", args, err)
	}
	parser = nil
}
