package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// a run's identity file, run-<token>.ident beside its lease: the boot id
// and pid namespace of the process that holds the lease. the lease says
// host and pid; the identity says which kernel's pid space that pid is in,
// so a sweep at the next run's start can tell for certain that the pid is
// gone (docs/spec/run-sweep.md). a separate file, b/c an older pats reads
// the lease's whole rest of line as its pid.

const identSuffix = ".ident"

// identity is a process's boot id and pid namespace ("-" where the os has
// none), "" when either cannot be read.
func identity() string {
	boot, ns := bootID(), pidNS()
	if boot == "" || ns == "" {
		return ""
	}
	return boot + " " + ns
}

// identPath is the identity file beside a lease file.
func identPath(lease string) string {
	return strings.TrimSuffix(lease, leaseSuffix) + identSuffix
}

// writeIdent writes this process's identity beside its lease; a process
// that cannot read one writes none, and so is never swept.
func writeIdent(lease string) error {
	id := identity()
	if id == "" {
		return nil
	}
	return os.WriteFile(identPath(lease), []byte(id+"\n"), 0o644)
}

// readIdent is the identity beside a lease, "" when there is none.
func readIdent(lease string) (string, error) {
	b, err := os.ReadFile(identPath(lease))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// removeIdent removes the identity beside a lease, if any.
func removeIdent(lease string) {
	_ = os.Remove(identPath(lease))
}
