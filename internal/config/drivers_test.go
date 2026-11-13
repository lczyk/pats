package config

import (
	"slices"
	"testing"

	"github.com/lczyk/pats/src/sandbox"
)

// needDriver skips a test of a driver this build leaves out.
func needDriver(t testing.TB, name string) {
	t.Helper()
	if !slices.Contains(sandbox.Drivers(), name) {
		t.Skipf("driver %q is not in this build", name)
	}
}
