package config

import (
	"os"
	"testing"
)

// builtHostSteps is what this build says of host steps, before the
// fixtures' override.
var builtHostSteps = hostSteps

// the fixtures run their steps on the host, in every build; the tests of
// the nohost rule put the build's own answer back.
func TestMain(m *testing.M) {
	AllowHostStepsInTests()
	// Load reads the machine's file: never the developer's own.
	dir, err := os.MkdirTemp("", "pats-config-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// withBuiltHostSteps runs the rest of a test with this build's own answer.
func withBuiltHostSteps(t *testing.T) {
	prev := hostSteps
	hostSteps = builtHostSteps
	t.Cleanup(func() { hostSteps = prev })
}
