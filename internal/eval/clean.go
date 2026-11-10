package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lczyk/pats/src/sandbox"
)

// CleanOptions configures Clean.
type CleanOptions struct {
	Drivers   []string      // container engines to sweep; nil: docker and podman, whichever are installed
	OlderThan time.Duration // a dead run's temp dirs younger than this are kept
	DryRun    bool
	// All also removes runs this clean cannot judge (another user's or
	// machine's on a shared daemon), what no run owns (lxd's uplink), every
	// image pats built or pulled, and dead runs' temp dirs of any age. a run
	// whose fresh lease this clean reads is never removed by it.
	All bool
	// Force is All, and also live runs, the containers using an image it
	// removes, and every temp dir and lease file: everything pats left.
	Force bool
	// Run removes that one run, whatever its lease.
	Run string
	// LXDRemotes are the lxd servers to sweep; nil: the lxc cli's default
	// remote, when lxc is installed and Drivers is nil.
	LXDRemotes []string
	// JSON writes each event as a one-line json object (sandbox.CleanEvent)
	// in place of a log line.
	JSON bool
	Out  io.Writer
}

// cleanReport renders clean's events to o.Out: log lines, a warning where
// something stays that the caller may want gone, or json lines.
func cleanReport(o CleanOptions) func(sandbox.CleanEvent) {
	var mu sync.Mutex
	if o.JSON {
		enc := json.NewEncoder(o.Out)
		return func(e sandbox.CleanEvent) {
			mu.Lock()
			defer mu.Unlock()
			_ = enc.Encode(e)
		}
	}
	lg := logw{o.Out, useColor(o.Out), 0}
	return func(e sandbox.CleanEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Action {
		case "keep", "skip", "warn":
			lg.warn("%s", e)
		default:
			lg.info("%s", e)
		}
	}
}

// pinnedHere is every image a registered project's pins say pats pulled or
// built on d's daemon: an image: ref pats pulled carries no mark of pats's
// on the daemon itself. a project whose pins cannot be read adds none.
func pinnedHere(ctx context.Context, d sandbox.Driver) []sandbox.Resource {
	t, err := d.Target(ctx)
	if err != nil {
		return nil
	}
	projects, _ := readProjects()
	var out []sandbox.Resource
	add := func(id, name string) {
		label := name
		if label == "" {
			label = short(id)
		}
		out = append(out, sandbox.Resource{Kind: "image", Name: id, Label: label})
	}
	for _, p := range projects {
		f, err := readPins(p)
		if err != nil {
			continue
		}
		for _, e := range f.Targets[t.Key] {
			if e.Daemon != t.Daemon {
				continue
			}
			for _, im := range e.images() {
				if im.owned() {
					add(im.ID, im.Name)
				}
			}
		}
		for _, s := range f.Superseded {
			if s.Target == t.Key && s.Daemon == t.Daemon && (s.Built || s.Pulled) {
				add(s.ID, s.Name)
			}
		}
	}
	return out
}

// openDriver opens a place to clean; a var so tests fake it.
var openDriver = sandbox.Open

// Clean removes what dead runs left behind, on every driver it sweeps and in
// the temp root, by the rule in sandbox.Clean. it holds init's machine lock
// while it removes, so the two never act on one daemon at once.
func Clean(ctx context.Context, o CleanOptions) error {
	places := sandbox.Places(o.Drivers, o.LXDRemotes)

	// leases the run sweep removes, or would: the temp sweep counts them gone.
	planned := map[string]bool{}
	render := cleanReport(o)
	report := func(e sandbox.CleanEvent) {
		if e.Action == "remove" && e.Kind == "lease" {
			planned[e.Name] = true
		}
		render(e)
	}
	unlock, err := machineLock(ctx)
	if err != nil {
		return fmt.Errorf("cannot take %s.lock: %w", projectsFile(), err)
	}
	defer unlock()
	var errs []error
	listed := map[string]bool{}
	complete := true
	for _, p := range places {
		d, err := openDriver(p.Options)
		if err != nil {
			errs = append(errs, err)
			complete = false
			continue
		}
		co := sandbox.CleanOptions{Place: p.Name, All: o.All, Force: o.Force, Run: o.Run, DryRun: o.DryRun, Report: report}
		if (o.All || o.Force) && sandbox.IsEngine(p.Options.Driver) {
			co.Pinned = pinnedHere(ctx, d)
		}
		runs, err := sandbox.Clean(ctx, d, co)
		for t := range runs {
			listed[t] = true
		}
		if err != nil {
			if runs == nil {
				// the place could not be listed: its daemon is down, or
				// the cli is not set up for it.
				report(sandbox.CleanEvent{Action: "skip", Place: p.Name, DryRun: o.DryRun, Why: err.Error()})
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
		olderThan := o.OlderThan
		if o.All || o.Force {
			olderThan = 0
		}
		if err := sandbox.CleanTemp(listed, planned, olderThan, o.DryRun, o.Force, report); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
