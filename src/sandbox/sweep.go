package sandbox

import (
	"context"
	"errors"
	"os"
	"sort"
)

// SweepOptions configures Sweep.
type SweepOptions struct {
	Place  string // what the output calls the driver's place
	DryRun bool
	Report func(CleanEvent) // nil: nothing is said
}

// Sweep stops what certainly dead runs of this machine left on d: their
// sandboxes, networks and acls, nothing else (docs/spec/run-sweep.md). a
// run is certainly dead when its lease and identity files are here, name
// this host, boot and pid namespace, and its pid is gone. everything else,
// and the rest of a swept run, is pats clean's.
func Sweep(ctx context.Context, d Driver, o SweepOptions) error {
	return defaultProcess.sweep(ctx, d, o)
}

// sweptKinds are what a sweep removes: what holds memory and cpus, and the
// networks that would hold them.
var sweptKinds = map[string]bool{"sandbox": true, "network": true, "acl": true}

func (p *process) sweep(ctx context.Context, d Driver, o SweepOptions) error {
	report := func(e CleanEvent) {
		if o.Report != nil {
			e.Place, e.DryRun = o.Place, o.DryRun
			o.Report(e)
		}
	}
	runs, err := d.Runs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range runs {
		if r.Token == "" || r.Token == p.token || !p.certainlyDead(r.Token) {
			continue
		}
		var res []Resource
		for _, x := range r.Resources {
			if sweptKinds[x.Kind] {
				res = append(res, x)
			}
		}
		sort.SliceStable(res, func(i, j int) bool { return order(res[i].Kind) < order(res[j].Kind) })
		for _, x := range res {
			if !o.DryRun {
				// in order: what is after a failed removal may be held by it.
				if err := d.Remove(ctx, x); err != nil {
					errs = append(errs, err)
					break
				}
			}
			e := CleanEvent{Action: "remove", Kind: x.Kind, Name: x.Name, Run: r.Token, Why: "its process is gone"}
			if x.Label != x.Name {
				e.Label = x.Label
			}
			report(e)
		}
	}
	return errors.Join(errs...)
}

// certainlyDead says whether the run's lease and identity files say it ran
// in this kernel's pid space and its pid is gone. a reused pid reads as
// alive, the safe way.
func (p *process) certainlyDead(token string) bool {
	path, err := p.leasePath(token)
	if err != nil {
		return false
	}
	l, ok, err := readLeaseAt(path)
	if err != nil || !ok {
		return false
	}
	host, _ := os.Hostname()
	if l.host != host || l.pid <= 0 {
		return false
	}
	id, err := readIdent(path)
	if err != nil || id == "" || id != identity() {
		return false
	}
	return !processAlive(l.pid)
}
