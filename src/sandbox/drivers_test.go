package sandbox

import (
	"context"
	"slices"
	"testing"

	"github.com/lczyk/assert"
)

func TestDriversRegistered(t *testing.T) {
	got := map[string]bool{}
	for _, d := range Drivers() {
		got[d] = true
	}
	// each engine and lxd are in every build.
	for _, d := range append(Engines(), LXDDriver) {
		assert.That(t, got[d], "driver %q not registered", d)
	}
}

func TestOpenTargetUnknown(t *testing.T) {
	for _, target := range []string{"", "nope:x"} {
		_, ok := OpenTarget(context.Background(), target, "")
		assert.That(t, !ok, "target %q reopened", target)
	}
}

func TestTraitsOfBoxed(t *testing.T) {
	for _, d := range append(Engines(), LXDDriver, "bwrap", "nope") {
		assert.Equal(t, TraitsOf(d), Traits{}, "driver %q", d)
	}
}

// needDriver skips a test of a driver this build leaves out.
func needDriver(t testing.TB, name string) {
	t.Helper()
	if !slices.Contains(Drivers(), name) {
		t.Skipf("driver %q is not in this build", name)
	}
}
