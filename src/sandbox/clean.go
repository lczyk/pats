package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pats clean removes what dead runs left, on every driver, by one rule: a
// run is alive while its lease is fresh, wherever the lease is kept (lxd's
// marker acl on the server, a lease file on the host that ran a container
// run). see docs/spec/sandbox-seams.md, "clean".

// RunRecord is what a driver holds of one run, unjudged.
type RunRecord struct {
	Token, Owner string // "" for what no run owns (lxd's shared uplink)
	Resources    []Resource
	// Held says the place keeps the run's lease itself (lxd), as Lease:
	// zero when the record is gone. otherwise the lease is a file on the
	// host that ran it.
	Held  bool
	Lease time.Time
}

// Resource is one thing a run left.
type Resource struct {
	Kind    string // sandbox | network | acl | alias | image | marker
	Name    string // what the driver removes it by
	Label   string // what clean prints
	Created time.Time
	// LastUsed is an image's last use, or its upload when never used.
	LastUsed time.Time
}

// unusedFor is how long an image a run left under only its private alias
// waits unused before clean takes it: a forced init may have re-pointed the
// public alias away from it while a pin elsewhere still names it.
const unusedFor = 30 * 24 * time.Hour

// CleanOptions configures Clean.
type CleanOptions struct {
	Place  string // what the output calls the driver's place: docker, lxd srv
	Force  bool   // also runs this clean cannot judge, and what no run owns
	Run    string // remove this run whatever its lease
	DryRun bool
	Out    io.Writer
}

// Clean removes what dead runs left on d, and returns the token of every
// run it listed, for the temp dirs and lease files beside them.
func Clean(ctx context.Context, d Driver, o CleanOptions) (map[string]bool, error) {
	return defaultProcess.clean(ctx, d, o)
}

// verdict is what clean makes of a run.
type verdict int

const (
	alive verdict = iota
	dead
	unknown
)

// judge is a run's verdict, and why.
func (p *process) judge(r RunRecord, now time.Time) (verdict, string) {
	stale := now.Add(-p.t.leaseStale)
	young := func() bool {
		for _, res := range r.Resources {
			if !res.Created.Before(stale) {
				return true
			}
		}
		return false
	}
	if r.Held {
		switch {
		case !r.Lease.IsZero() && r.Lease.Before(stale):
			return dead, "its lease is stale"
		case !r.Lease.IsZero():
			return alive, ""
		case young():
			return alive, "it has no lease yet"
		}
		return dead, "it has no lease"
	}
	l, ok, err := p.readLease(r.Token)
	switch {
	case err != nil:
		return unknown, err.Error()
	case ok:
		host, _ := os.Hostname()
		if l.host == host && l.pid > 0 && !processAlive(l.pid) {
			return dead, "its process is gone"
		}
		if l.at.Before(stale) {
			return dead, "its lease is stale"
		}
		return alive, ""
	case r.Owner != p.owner():
		return unknown, "another user's or machine's"
	case young():
		return alive, "it has no lease yet"
	}
	return dead, "it has no lease"
}

// order is the removal order: sandboxes first (one holds its network),
// then networks and acls, then a run's private aliases and images, and the
// record last, so a clean that stops halfway leaves the run still judged.
func order(kind string) int {
	switch kind {
	case "sandbox":
		return 0
	case "network", "acl":
		return 1
	case "alias":
		return 2
	case "image":
		return 3
	}
	return 9
}

func (p *process) clean(ctx context.Context, d Driver, o CleanOptions) (map[string]bool, error) {
	runs, err := d.Runs(ctx)
	if err != nil {
		return nil, err
	}
	// what no run owns last: its users go first.
	sort.Slice(runs, func(i, j int) bool {
		if (runs[i].Token == "") != (runs[j].Token == "") {
			return runs[j].Token == ""
		}
		return runs[i].Token < runs[j].Token
	})
	now := p.clock.Now()
	act := "removed"
	if o.DryRun {
		act = "would remove"
	}
	listed := map[string]bool{}
	var errs []error
	live, foreign := 0, 0
	for _, r := range runs {
		listed[r.Token] = true
		var v verdict
		var why string
		switch {
		case r.Token == "":
			v, why = dead, "no run uses it"
		case o.Run != "":
			v, why = dead, "asked for"
		default:
			v, why = p.judge(r, now)
		}
		switch {
		case r.Token == "" && !o.Force:
			continue
		case o.Run != "" && r.Token != o.Run:
			continue
		case v == alive:
			live++
			continue
		case v == unknown && !o.Force:
			foreign++
			continue
		case v == unknown:
			fmt.Fprintf(o.Out, "%s: run %s (%s) may be alive elsewhere (%s); --force takes it\n", o.Place, r.Token, r.Owner, why)
		}
		res := append([]Resource(nil), r.Resources...)
		sort.SliceStable(res, func(i, j int) bool { return order(res[i].Kind) < order(res[j].Kind) })
		failed := false
		for _, x := range res {
			if x.Kind == "image" && now.Sub(x.LastUsed) < unusedFor {
				continue // a pin may still name it; the next clean judges it again
			}
			if order(x.Kind) == 9 && failed {
				break // the record stays while anything of the run does
			}
			if !o.DryRun {
				if err := d.Remove(ctx, x); err != nil {
					errs = append(errs, err)
					failed = true
					continue
				}
			}
			run := r.Token
			if run == "" {
				run = "shared"
			}
			fmt.Fprintf(o.Out, "%s %s %s %s (run %s: %s)\n", act, o.Place, x.Kind, x.Label, run, why)
		}
		if !r.Held && r.Token != "" && !failed && !o.DryRun {
			if path, err := p.leasePath(r.Token); err == nil {
				_ = os.Remove(path)
			}
		}
	}
	if live > 0 {
		fmt.Fprintf(o.Out, "%s: left %d live run(s); pats clean --run <token> removes one\n", o.Place, live)
	}
	if foreign > 0 {
		fmt.Fprintf(o.Out, "%s: left %d run(s) this clean cannot judge (another user, machine or cache dir); --force removes them\n", o.Place, foreign)
	}
	return listed, errors.Join(errs...)
}

// CleanTemp removes the temp dirs and lease files runs left on this host:
// a temp dir once its run is dead and it is older than olderThan, a lease
// file once stale and naming no run in listed and no temp dir. a nil listed
// (a place could not be listed) leaves every lease file.
func CleanTemp(listed map[string]bool, olderThan time.Duration, dryRun bool, out io.Writer) error {
	return defaultProcess.cleanTemp(listed, olderThan, dryRun, out)
}

func (p *process) cleanTemp(listed map[string]bool, olderThan time.Duration, dryRun bool, out io.Writer) error {
	dir, err := p.home()
	if err != nil {
		return err
	}
	root := filepath.Join(dir, "tmp")
	act := "removed"
	if dryRun {
		act = "would remove"
	}
	now := p.clock.Now()
	var errs []error
	held := map[string]bool{} // runs a temp dir names
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range ents {
		path := filepath.Join(root, e.Name())
		if owner, ok := strings.CutSuffix(path, ownerSuffix); ok {
			// an owner file outlives its dir when the caller removed the dir.
			if _, err := os.Stat(owner); os.IsNotExist(err) && !dryRun {
				_ = os.Remove(path)
			}
			continue
		}
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		token, alive := p.tempAlive(path, now)
		held[token] = true
		if alive || now.Sub(info.ModTime()) < olderThan {
			continue
		}
		if !dryRun {
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, err)
				continue
			}
			_ = os.Remove(path + ownerSuffix)
		}
		fmt.Fprintf(out, "%s %s\n", act, path)
	}
	if listed == nil {
		return errors.Join(errs...)
	}
	leases, _ := filepath.Glob(filepath.Join(dir, leasePrefix+"*"+leaseSuffix))
	for _, path := range leases {
		token := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), leasePrefix), leaseSuffix)
		if !isHex(token) || token == "" || listed[token] || held[token] {
			continue
		}
		l, ok, err := p.readLease(token)
		if err != nil || !ok || !l.at.Before(now.Add(-p.t.leaseStale)) {
			continue
		}
		if !dryRun {
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		fmt.Fprintf(out, "%s %s\n", act, path)
	}
	return errors.Join(errs...)
}

// tempAlive is the run a temp dir's owner file names, and whether that run
// is alive: its lease fresh and, on this host, its process there. an owner
// file of a pats before leases holds a pid, judged by that alone.
func (p *process) tempAlive(dir string, now time.Time) (token string, live bool) {
	b, err := os.ReadFile(dir + ownerSuffix)
	if err != nil {
		return "", false
	}
	token = strings.TrimSpace(string(b))
	if len(token) != len(p.token) || !isHex(token) {
		pid, err := strconv.Atoi(token)
		return "", err == nil && pid > 0 && processAlive(pid)
	}
	v, _ := p.judge(RunRecord{Token: token, Owner: p.owner()}, now)
	return token, v == alive
}
