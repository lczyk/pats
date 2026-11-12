package sandbox

import (
	"context"
	"sort"
	"strings"
)

// driverDef is what a driver registers from its own file, so a build tag that
// leaves the file out leaves the driver out, and nothing else names it.
type driverDef struct {
	// open makes the driver o names on p, its cli run through r when r is
	// set (tests), else through the driver's own.
	open func(p *process, r runner, o Options) Driver
	// reopen is the driver a pin target names, rest being what follows
	// its kind; ok is false when this process cannot reach it as named.
	// nil: no pin names the driver.
	reopen func(ctx context.Context, target, rest, daemon string) (d Driver, ok bool)
	// sweep, when set, takes what a crashed run of the driver left in the
	// cache, reporting each removal.
	sweep func(dryRun bool, report func(CleanEvent))
	// purge, when set, takes what the driver keeps of the harness clis in
	// the cache, reporting each removal and what it left: clean --all's.
	purge  func(dryRun bool, report func(CleanEvent)) error
	traits Traits
}

// Traits are what callers outside a driver need to know of it, asked
// before any driver is open, so they never branch on its name.
type Traits struct {
	// Unboxed: the agent runs on this machine, egress only advisory.
	Unboxed bool
	// NoImages: nothing to pin, pull or build.
	NoImages bool
	// HostCLIs: a harness cli comes from this machine's PATH.
	HostCLIs bool
}

// TraitsOf are driver's traits; the zero value for one this build lacks.
func TraitsOf(driver string) Traits { return drivers[driver].traits }

var drivers = map[string]driverDef{}

func registerDriver(name string, d driverDef) { drivers[name] = d }

// Drivers are the drivers this build has, sorted.
func Drivers() []string {
	out := make([]string, 0, len(drivers))
	for name := range drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// reopenSame opens kind with this process's options and holds it to the
// pinned target: the same key, and the same daemon answering.
func reopenSame(ctx context.Context, target, _, daemon string) (Driver, bool) {
	kind, _, _ := strings.Cut(target, ":")
	d, err := Open(Options{Driver: kind}.WithEnv())
	if err != nil {
		return nil, false
	}
	now, err := d.Target(ctx)
	return d, err == nil && now.Key == target && now.Daemon == daemon
}
