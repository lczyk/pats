package eval

import (
	"testing"

	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/machine"
)

// hd is a default harness entry, the test's own copy; nil for an unknown
// name.
func hd(name string) *machine.Harness { return machine.Defaults().Harness(name) }

// harnessNames lists the default harnesses.
func harnessNames() []string { return machine.Defaults().Names() }

// tables parses harness tables of a test's own.
func tables(t *testing.T, src string) *machine.Tables {
	t.Helper()
	tb, err := machine.Parse([]byte(src))
	require.NoError(t, err)
	return tb
}
