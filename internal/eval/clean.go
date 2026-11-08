package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/lczyk/pats/src/sandbox"
)

// CleanOptions configures Clean.
type CleanOptions struct {
	Drivers   []string      // container engines to sweep; nil: docker and podman, whichever are installed
	OlderThan time.Duration // a dead run's temp dirs younger than this are kept
	DryRun    bool
	// Force also removes runs this clean cannot judge (another user's or
	// machine's on a shared daemon), and what no run owns (lxd's uplink).
	// a run whose fresh lease this clean reads is never removed by it.
	Force bool
	// Run removes that one run, whatever its lease.
	Run string
	// LXDRemotes are the lxd servers to sweep; nil: the lxc cli's default
	// remote, when lxc is installed and Drivers is nil.
	LXDRemotes []string
	Out        io.Writer
}

// openDriver opens a place to clean; a var so tests fake it.
var openDriver = sandbox.Open

// Clean removes what dead runs left behind, on every driver it sweeps and in
// the temp root, by the rule in sandbox.Clean. it holds init's machine lock
// while it removes, so the two never act on one daemon at once.
func Clean(ctx context.Context, o CleanOptions) error {
	type place struct {
		name string
		o    sandbox.Options
	}
	var places []place
	drivers := o.Drivers
	if drivers == nil {
		for _, d := range sandbox.Engines() {
			if _, err := exec.LookPath(d); err == nil {
				drivers = append(drivers, d)
			}
		}
	}
	for _, d := range drivers {
		places = append(places, place{d, sandbox.Options{Driver: d}.WithEnv()})
	}
	remotes := o.LXDRemotes
	if remotes == nil && o.Drivers == nil {
		if _, err := exec.LookPath("lxc"); err == nil {
			remotes = []string{""}
		}
	}
	for _, r := range remotes {
		name := "lxd"
		if r != "" {
			name += " " + r
		}
		places = append(places, place{name, sandbox.Options{Driver: "lxd", Remote: r}.WithEnv()})
	}

	unlock, err := machineLock()
	if err != nil {
		return fmt.Errorf("cannot take %s.lock: %w", projectsFile(), err)
	}
	defer unlock()
	var errs []error
	listed := map[string]bool{}
	complete := true
	for _, p := range places {
		d, err := openDriver(p.o)
		if err != nil {
			errs = append(errs, err)
			complete = false
			continue
		}
		runs, err := sandbox.Clean(ctx, d, sandbox.CleanOptions{Place: p.name, Force: o.Force, Run: o.Run, DryRun: o.DryRun, Out: o.Out})
		for t := range runs {
			listed[t] = true
		}
		if err != nil {
			if runs == nil {
				// the place could not be listed: its daemon is down, or
				// the cli is not set up for it.
				fmt.Fprintf(o.Out, "%s: skipped (%v)\n", p.name, err)
				complete = false
				continue
			}
			errs = append(errs, err)
		}
	}
	if o.Run == "" {
		// a lease file names a run a skipped place may still hold.
		if !complete {
			listed = nil
		}
		if err := sandbox.CleanTemp(listed, o.OlderThan, o.DryRun, o.Out); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
