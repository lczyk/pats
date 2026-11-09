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
	// Holders are the run-private aliases on a cached image (lxd): it is a
	// run's while any of them stays.
	Holders []string
}

// unusedFor is how long an image a run left under only its private alias
// waits unused before clean takes it: a forced init may have re-pointed the
// public alias away from it while a pin elsewhere still names it.
const unusedFor = 30 * 24 * time.Hour

// CleanOptions configures Clean.
type CleanOptions struct {
	Place string // what the output calls the driver's place: docker, lxd srv
	// All also takes runs this clean cannot judge, what no run owns, and
	// every image pats made on the place (see imageCache).
	All bool
	// Force is All, and also live runs, the containers using an image it
	// removes, and every temp dir and lease file.
	Force bool
	Run   string // remove this run whatever its lease
	// Pinned are images the projects' pins say pats pulled or built here,
	// for All beside what the place itself knows as pats's.
	Pinned []Resource
	DryRun bool
	Report func(CleanEvent) // nil: nothing is said
}

// CleanEvent is one thing clean did, would do (a dry run), or left, for its
// caller to render as a log line or a json object.
type CleanEvent struct {
	Action string `json:"action"` // remove | keep | leave | skip | warn
	DryRun bool   `json:"dry_run"`
	Place  string `json:"place"` // docker, lxd srv; host for temp dirs and leases
	// Kind is a resource's (sandbox, network, acl, alias, image, marker,
	// temp, lease, owner), or what a leave is about (live, unknown runs; the
	// legacy cache dir).
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"` // Name as the text says it, when that differs
	Run   string `json:"run,omitempty"`
	Owner string `json:"owner,omitempty"`
	Why   string `json:"why,omitempty"`
	Count int    `json:"count,omitempty"`
}

// String is the event as a log line, without the level.
func (e CleanEvent) String() string {
	name := e.Name
	if e.Label != "" {
		name = e.Label
	}
	switch e.Action {
	case "remove":
		act := "removed"
		if e.DryRun {
			act = "would remove"
		}
		switch e.Kind {
		case "temp":
			return act + " temp dir " + name
		case "lease":
			return act + " lease " + name
		case "owner":
			return act + " owner file " + name
		}
		why := e.Why
		if e.Run != "" {
			why = "run " + e.Run + ": " + why
		}
		return fmt.Sprintf("%s %s %s %s (%s)", act, e.Place, e.Kind, name, why)
	case "keep":
		return fmt.Sprintf("%s: kept %s %s (%s)", e.Place, e.Kind, name, e.Why)
	case "leave":
		if e.Kind == "legacy" {
			return fmt.Sprintf("left legacy cache dir %s: it holds only an old client id; remove it once no older pats is installed", name)
		}
		if e.Kind == "live" {
			return fmt.Sprintf("%s: left %d live run(s); pats clean --run <token> removes one", e.Place, e.Count)
		}
		return fmt.Sprintf("%s: left %d run(s) this clean cannot judge (another user, machine or cache dir); --all removes them", e.Place, e.Count)
	case "skip":
		return fmt.Sprintf("%s: skipped (%s)", e.Place, e.Why)
	case "warn":
		return fmt.Sprintf("%s: run %s (%s) may be alive elsewhere (%s); --all takes it", e.Place, e.Run, e.Owner, e.Why)
	}
	return e.Action + " " + name
}

// TextReport writes each event to w as its String, one a line.
func TextReport(w io.Writer) func(CleanEvent) {
	return func(e CleanEvent) { fmt.Fprintln(w, e.String()) }
}

func (o CleanOptions) report(e CleanEvent) {
	if o.Report != nil {
		e.Place, e.DryRun = o.Place, o.DryRun
		o.Report(e)
	}
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
	case r.Owner != p.knownOwner():
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
	if o.Force {
		o.All = true
	}
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
	listed := map[string]bool{}
	gone := map[string]bool{} // "<kind> <name>" this clean removes, or would
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
		case r.Token == "" && !o.All:
			continue
		case r.Token == "" && live+foreign > 0:
			// the place refuses it while a run that stays is on it; said
			// up front, so a dry run says what a real one does.
			for _, x := range r.Resources {
				o.report(CleanEvent{Action: "keep", Kind: x.Kind, Name: x.Name, Label: x.Label, Why: "a run left here may use it"})
			}
			continue
		case o.Run != "" && r.Token != o.Run:
			continue
		case v == alive && o.Force:
			why = "alive, forced"
		case v == alive:
			live++
			continue
		case v == unknown && !o.All:
			foreign++
			continue
		case v == unknown:
			o.report(CleanEvent{Action: "warn", Run: r.Token, Owner: r.Owner, Why: why})
		}
		res := append([]Resource(nil), r.Resources...)
		sort.SliceStable(res, func(i, j int) bool { return order(res[i].Kind) < order(res[j].Kind) })
		failed := false
		for _, x := range res {
			if x.Kind == "image" && !o.All && now.Sub(x.LastUsed) < unusedFor {
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
			gone[x.Kind+" "+x.Name] = true
			e := CleanEvent{Action: "remove", Kind: x.Kind, Name: x.Name, Run: r.Token, Why: why}
			if x.Label != x.Name {
				e.Label = x.Label
			}
			o.report(e)
		}
		if !r.Held && r.Token != "" && !failed {
			if path, err := p.leaseFound(r.Token); err == nil {
				if _, err := os.Stat(path); err == nil {
					if !o.DryRun {
						_ = os.Remove(path)
					}
					o.report(CleanEvent{Action: "remove", Kind: "lease", Name: path, Run: r.Token, Why: why})
				}
			}
		}
	}
	if o.All && o.Run == "" {
		if c, ok := d.(imageCache); ok {
			if err := cleanCached(ctx, d, c, o, gone); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if live > 0 {
		o.report(CleanEvent{Action: "leave", Kind: "live", Count: live})
	}
	if foreign > 0 {
		o.report(CleanEvent{Action: "leave", Kind: "unknown", Count: foreign})
	}
	return listed, errors.Join(errs...)
}

// imageCache is a place holding images pats made that outlive any run:
// init's builds and pulls. only clean --all takes them.
type imageCache interface {
	CachedImages(ctx context.Context) ([]Resource, error)
	// ImageUsers names the containers an image, or one built on it, runs
	// or ran in: the place refuses to remove it while any is there.
	ImageUsers(ctx context.Context, id string) ([]string, error)
	// ImageChildren maps an image to the images built on it: the place
	// refuses to remove it while one stays.
	ImageChildren(ctx context.Context) (map[string][]Resource, error)
}

// cleanCached removes every cached image, in passes, since an image goes
// only after the ones built on it. one that stays (a container uses it) is
// said, not an error.
func cleanCached(ctx context.Context, d Driver, c imageCache, o CleanOptions, gone map[string]bool) error {
	imgs, err := c.CachedImages(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, x := range imgs {
		seen[x.Name] = true
	}
	// a pin outlives its image: only what the place still holds is news.
	var held map[string]bool
	if len(o.Pinned) > 0 {
		if held, err = d.Images(ctx); err != nil {
			return err
		}
	}
	for _, x := range o.Pinned {
		if !seen[x.Name] && held[x.Name] {
			seen[x.Name] = true
			imgs = append(imgs, x)
		}
	}
	// one in use is kept up front, so a dry run says what a real one does.
	var free []Resource
next:
	for _, x := range imgs {
		if gone["image "+x.Name] {
			continue // a run's, taken with it
		}
		for _, h := range x.Holders {
			if !gone["alias "+h] {
				continue next // a run's that stays
			}
		}
		all, err := c.ImageUsers(ctx, x.Name)
		if err != nil {
			return err
		}
		var users []string // those no earlier step takes
		for _, u := range all {
			if !gone["sandbox "+u] {
				users = append(users, u)
			}
		}
		if len(users) == 0 {
			free = append(free, x)
			continue
		}
		if o.Force {
			for _, u := range users {
				gone["sandbox "+u] = true
				if !o.DryRun {
					if err := d.Remove(ctx, Resource{Kind: "sandbox", Name: u}); err != nil {
						return err
					}
				}
				o.report(CleanEvent{Action: "remove", Kind: "sandbox", Name: u, Why: "it uses image " + x.Label + ", forced"})
			}
			free = append(free, x)
			continue
		}
		e := CleanEvent{Action: "keep", Kind: "image", Name: x.Name, Why: "used by " + strings.Join(users, ", ")}
		if x.Label != x.Name {
			e.Label = x.Label
		}
		o.report(e)
	}
	// one with an image built on it that stays, stays too: someone's own
	// image FROM it, or one kept above. until nothing more is kept.
	kids, err := c.ImageChildren(ctx)
	if err != nil {
		return err
	}
	for changed := true; changed; {
		changed = false
		taken := map[string]bool{}
		for _, x := range free {
			taken[x.Name] = true
		}
		var still []Resource
		for _, x := range free {
			var stays []string
			for _, k := range kids[x.Name] {
				if !taken[k.Name] {
					stays = append(stays, k.Label)
				}
			}
			if len(stays) == 0 {
				still = append(still, x)
				continue
			}
			changed = true
			e := CleanEvent{Action: "keep", Kind: "image", Name: x.Name, Why: "built on by " + strings.Join(stays, ", ")}
			if x.Label != x.Name {
				e.Label = x.Label
			}
			o.report(e)
		}
		free = still
	}
	imgs = free
	// removed in passes, said in listing order: what a dry run says too.
	failed := map[string]error{}
	if !o.DryRun {
		for pending := imgs; len(pending) > 0; {
			var next []Resource
			for _, x := range pending {
				if err := d.Remove(ctx, x); err != nil {
					failed[x.Name] = err
					next = append(next, x)
					continue
				}
				delete(failed, x.Name)
			}
			if len(next) == len(pending) {
				break
			}
			pending = next
		}
	}
	for _, x := range imgs {
		e := CleanEvent{Action: "remove", Kind: "image", Name: x.Name, Why: "cached"}
		if err := failed[x.Name]; err != nil {
			msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
			e.Action, e.Why = "keep", msg
		}
		if x.Label != x.Name {
			e.Label = x.Label
		}
		o.report(e)
	}
	return nil
}

// CleanTemp removes the temp dirs and lease files runs left on this host:
// a temp dir once its run is dead and it is older than olderThan, a lease
// file once stale and naming no run in listed and no temp dir. a nil listed
// (a place could not be listed) leaves every lease file.
// planned names what the run sweep removed, or would: a dry run leaves it on
// disk, and it counts as gone for the legacy dir's note all the same.
func CleanTemp(listed, planned map[string]bool, olderThan time.Duration, dryRun, force bool, report func(CleanEvent)) error {
	return defaultProcess.cleanTemp(listed, planned, olderThan, dryRun, force, report)
}

func (p *process) cleanTemp(listed, planned map[string]bool, olderThan time.Duration, dryRun, force bool, report func(CleanEvent)) error {
	o := CleanOptions{Place: "host", DryRun: dryRun, Report: report}
	dir, err := p.home()
	if err != nil {
		return err
	}
	_, err = p.cleanTempIn(dir, o, listed, olderThan, force)
	errs := []error{err}
	// an older pats's home: swept by the same rules, and never removed (an
	// old binary may hold its lock), only pointed at once nothing else is in it.
	if legacy := p.legacy(); legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			gone, err := p.cleanTempIn(legacy, o, listed, olderThan, force)
			errs = append(errs, err)
			for path := range planned {
				gone[path] = true
			}
			if onlyClientID(legacy, gone) {
				o.report(CleanEvent{Action: "leave", Kind: "legacy", Name: legacy})
			}
		}
	}
	return errors.Join(errs...)
}

// onlyClientID says whether dir holds nothing but a client id, its lock and
// an empty tmp/, once what gone names is gone.
func onlyClientID(dir string, gone map[string]bool) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		switch {
		case gone[path], e.Name() == "client-id", e.Name() == "client-id.lock":
		case e.Name() == "tmp" && e.IsDir():
			sub, err := os.ReadDir(path)
			if err != nil {
				return false
			}
			for _, s := range sub {
				if !gone[filepath.Join(path, s.Name())] {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// cleanTempIn sweeps one home's temp root and lease files, and returns what
// it removed, or would in a dry run.
func (p *process) cleanTempIn(dir string, o CleanOptions, listed map[string]bool, olderThan time.Duration, force bool) (map[string]bool, error) {
	dryRun := o.DryRun
	root := filepath.Join(dir, "tmp")
	now := p.clock.Now()
	var errs []error
	held := map[string]bool{} // runs a temp dir names
	gone := map[string]bool{} // what this sweep takes: dirs, owner files, leases
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return gone, err
	}
	for _, e := range ents {
		path := filepath.Join(root, e.Name())
		if owner, ok := strings.CutSuffix(path, ownerSuffix); ok {
			// an owner file outlives its dir when the caller removed the dir.
			if _, err := os.Stat(owner); os.IsNotExist(err) && !gone[owner] {
				if !dryRun {
					_ = os.Remove(path)
				}
				gone[path] = true
				o.report(CleanEvent{Action: "remove", Kind: "owner", Name: path})
			}
			continue
		}
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		token, alive := p.tempAlive(path, now)
		held[token] = true
		if !force && (alive || now.Sub(info.ModTime()) < olderThan) {
			continue
		}
		if !dryRun {
			// removeTree: a bwrap session's overlay work dir is mode 000.
			if err := removeTree(path); err != nil {
				errs = append(errs, err)
				continue
			}
			_ = os.Remove(path + ownerSuffix)
		}
		gone[path], gone[path+ownerSuffix] = true, true
		o.report(CleanEvent{Action: "remove", Kind: "temp", Name: path})
	}
	// a bwrap unpack a crash cut short leaves <id>.new, taken only when no
	// unpack holds its lock.
	if st, err := newBwrapStore(); err == nil {
		for _, name := range st.sweepNew(dryRun) {
			o.report(CleanEvent{Action: "remove", Kind: "temp", Name: filepath.Join(st.dir, "rootfs", name)})
		}
	}
	if listed == nil && !force {
		return gone, errors.Join(errs...)
	}
	leases, _ := filepath.Glob(filepath.Join(dir, leasePrefix+"*"+leaseSuffix))
	for _, path := range leases {
		token := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), leasePrefix), leaseSuffix)
		// a listed run's lease is the run sweep's, forced or not.
		if !isHex(token) || token == "" || listed[token] || (!force && held[token]) {
			continue
		}
		l, ok, err := readLeaseAt(path)
		if err != nil || !ok || (!force && !l.at.Before(now.Add(-p.t.leaseStale))) {
			continue
		}
		if !dryRun {
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		gone[path] = true
		o.report(CleanEvent{Action: "remove", Kind: "lease", Name: path})
	}
	return gone, errors.Join(errs...)
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
	// a run that cannot be judged (its lease unreadable) keeps its dirs.
	v, _ := p.judge(RunRecord{Token: token, Owner: p.knownOwner()}, now)
	return token, v != dead
}
