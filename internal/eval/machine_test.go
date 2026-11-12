package eval

import "github.com/lczyk/pats/internal/machine"

// hd is a default harness entry, the test's own copy; nil for an unknown
// name.
func hd(name string) *machine.Harness { return machine.Defaults().Harness(name) }

// harnessNames lists the default harnesses.
func harnessNames() []string { return machine.Defaults().Names() }
