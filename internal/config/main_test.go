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
	os.Exit(m.Run())
}

// withBuiltHostSteps runs the rest of a test with this build's own answer.
func withBuiltHostSteps(t *testing.T) {
	prev := hostSteps
	hostSteps = builtHostSteps
	t.Cleanup(func() { hostSteps = prev })
}
