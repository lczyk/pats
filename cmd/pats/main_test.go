package main

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

func TestWantsVersion(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-v"}, true},
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
