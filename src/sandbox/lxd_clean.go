package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CleanLXD removes what dead runs left on an lxd server ("" is the cli's
// default remote). a run is everything carrying its user.pats.run token; it is
// dead once its marker's lease is older than leaseStale, or, with no marker
// left, once the newest of its resources is. a live run is left whoever owns
// it, since its owner may be another machine. force takes every run's leftovers
// and the shared uplink, live or not; cached images stay either way, being a
// cache, not a leftover.
//
// liveness never rests on two clocks agreeing: a run whose lease looks stale
// by this machine's clock is only removed if the lease is still the same
// after waiting longer than a live run takes to renew it.
func CleanLXD(ctx context.Context, remote string, force, dryRun bool, out io.Writer) error {
	l := newLXD(Options{Remote: remote})
	wait := func() {
		fmt.Fprintf(out, "lxd %s: waiting %s to see whether stale-looking runs renew their lease\n", remote, leaseEvery+30*time.Second)
		select {
		case <-ctx.Done():
		case <-time.After(leaseEvery + 30*time.Second):
		}
	}
	return l.clean(ctx, force, dryRun, out, time.Now(), wait)
}

// lxdResource is one run-owned thing on the server.
type lxdResource struct {
	kind    string // instance | network | acl | image
	name    string // the instance, network or acl name, or an image's fingerprint
	label   string // what to print
	token   string
	created int64
	lease   int64 // a marker's lease, 0 elsewhere
	marker  bool
}

func (l *lxd) clean(ctx context.Context, force, dryRun bool, out io.Writer, now time.Time, wait func()) error {
	if l.o.Remote == "" {
		r, err := l.lxc(ctx, "remote", "get-default")
		if err != nil {
			return err
		}
		l.o.Remote = r
	}
	res, err := l.runResources(ctx)
	if err != nil {
		return err
	}
	runs := map[string][]lxdResource{}
	for _, r := range res {
		runs[r.token] = append(runs[r.token], r)
	}
	tokens := make([]string, 0, len(runs))
	for t := range runs {
		tokens = append(tokens, t)
	}
	sort.Strings(tokens)
	act := "removed"
	if dryRun {
		act = "would remove"
	}
	var errs []string
	live := 0
	var dead []string
	for _, tok := range tokens {
		if force || runDead(runs[tok], now) {
			dead = append(dead, tok)
		} else {
			live++
		}
	}
	// a stale-looking lease must also hold still: a live run renews it
	// within leaseEvery whatever either clock says.
	if !force && !dryRun && len(dead) > 0 && hasMarker(runs, dead) {
		wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		again, err := l.runResources(ctx)
		if err != nil {
			return err
		}
		leases := map[string]int64{}
		for _, r := range again {
			if r.marker {
				leases[r.token] = r.lease
			}
		}
		kept := dead[:0]
		for _, tok := range dead {
			if m := marker(runs[tok]); m != nil && leases[tok] != m.lease {
				live++ // renewed while we waited: alive, whatever the clocks say
				continue
			}
			kept = append(kept, tok)
		}
		dead = kept
	}
	for _, tok := range dead {
		group := runs[tok]
		// instances first (an attached nic holds its bridge), then
		// bridges, acls, images, and the marker last: a clean that dies
		// halfway leaves the run still marked or judged by creation time.
		sort.SliceStable(group, func(i, j int) bool { return cleanOrder(group[i]) < cleanOrder(group[j]) })
		for _, r := range group {
			if !dryRun {
				if err := l.remove(ctx, r); err != nil {
					errs = append(errs, err.Error())
					continue
				}
			}
			fmt.Fprintf(out, "%s lxd %s %s (run %s)\n", act, r.kind, r.label, tok)
		}
	}
	if force {
		if _, err := l.lxc(ctx, "network", "show", l.ref(lxdUplink), "--project", "default"); err == nil {
			if !dryRun {
				if _, err := l.lxc(ctx, "network", "delete", l.ref(lxdUplink), "--project", "default"); err != nil {
					errs = append(errs, err.Error())
				}
			}
			fmt.Fprintf(out, "%s lxd network %s (shared uplink)\n", act, lxdUplink)
		}
	}
	if live > 0 {
		fmt.Fprintf(out, "lxd %s: left %d live run(s) (lease renewed within %s); --force removes them\n", l.o.Remote, live, leaseStale)
	}
	if len(errs) > 0 {
		return fmt.Errorf("lxd clean: %s", strings.Join(errs, "; "))
	}
	return nil
}

func marker(group []lxdResource) *lxdResource {
	for i := range group {
		if group[i].marker {
			return &group[i]
		}
	}
	return nil
}

func hasMarker(runs map[string][]lxdResource, toks []string) bool {
	for _, t := range toks {
		if marker(runs[t]) != nil {
			return true
		}
	}
	return false
}

// runDead says whether a run's group may go: its marker's lease is stale, or
// it has no marker and nothing of it is younger than leaseStale.
func runDead(group []lxdResource, now time.Time) bool {
	cut := now.Add(-leaseStale).Unix()
	for _, r := range group {
		if r.marker {
			return r.lease < cut
		}
	}
	for _, r := range group {
		if r.created >= cut {
			return false
		}
	}
	return true
}

func cleanOrder(r lxdResource) int {
	switch {
	case r.marker:
		return 9
	case r.kind == "instance":
		return 0
	case r.kind == "network":
		return 1
	case r.kind == "acl":
		return 2
	default:
		return 3
	}
}

func (l *lxd) remove(ctx context.Context, r lxdResource) error {
	var err error
	switch r.kind {
	case "instance":
		_, err = l.lxc(ctx, "delete", l.ref(r.name), "--project", lxdProject, "--force")
	case "network":
		_, err = l.lxc(ctx, "network", "delete", l.ref(r.name), "--project", "default")
	case "acl":
		_, err = l.lxc(ctx, "network", "acl", "delete", l.ref(r.name), "--project", "default")
	case "image":
		_, err = l.lxc(ctx, "image", "delete", l.ref(r.name), "--project", lxdProject)
	}
	return err
}

// runResources lists everything on the server a pats run made: instances
// and images in the pats project, bridges and acls in the default one. a
// cached image (one a pats-img- alias names without a run suffix) is not a
// run's, even though the run that built it stamped it.
func (l *lxd) runResources(ctx context.Context) ([]lxdResource, error) {
	var res []lxdResource
	stamp := func(cfg map[string]string) (string, int64, bool) {
		tok := cfg["user.pats.run"]
		created, _ := strconv.ParseInt(cfg["user.pats.created"], 10, 64)
		return tok, created, tok != ""
	}
	var insts []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := l.query(ctx, "/1.0/instances?project="+lxdProject+"&recursion=1", &insts); err != nil {
		// no pats project: nothing of pats's is there.
		if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
	}
	for _, i := range insts {
		if tok, created, ok := stamp(i.Config); ok {
			res = append(res, lxdResource{kind: "instance", name: i.Name, label: i.Name, token: tok, created: created})
		}
	}
	var nets []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := l.query(ctx, "/1.0/networks?project=default&recursion=1", &nets); err != nil {
		return nil, err
	}
	for _, n := range nets {
		if tok, created, ok := stamp(n.Config); ok && strings.HasPrefix(n.Name, "patsbr") {
			res = append(res, lxdResource{kind: "network", name: n.Name, label: n.Name, token: tok, created: created})
		}
	}
	var acls []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := l.query(ctx, "/1.0/network-acls?project=default&recursion=1", &acls); err != nil {
		return nil, err
	}
	for _, a := range acls {
		tok, created, ok := stamp(a.Config)
		if !ok {
			continue
		}
		r := lxdResource{kind: "acl", name: a.Name, label: a.Name, token: tok, created: created}
		if strings.HasPrefix(a.Name, "pats-run-") {
			r.marker = true
			r.lease, _ = strconv.ParseInt(a.Config["user.pats.lease"], 10, 64)
		} else if !strings.HasPrefix(a.Name, "patsbr") {
			continue
		}
		res = append(res, r)
	}
	var imgs []struct {
		Fingerprint string            `json:"fingerprint"`
		Properties  map[string]string `json:"properties"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := l.query(ctx, "/1.0/images?project="+lxdProject+"&recursion=1", &imgs); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
	}
	for _, im := range imgs {
		tok, created, ok := stamp(im.Properties)
		if !ok {
			continue
		}
		cached := false
		for _, a := range im.Aliases {
			if strings.HasPrefix(a.Name, "pats-img-") && !strings.HasSuffix(a.Name, "-"+tok) {
				cached = true
			}
		}
		if cached {
			continue
		}
		label := im.Fingerprint[:12]
		for _, a := range im.Aliases {
			label = a.Name
		}
		res = append(res, lxdResource{kind: "image", name: im.Fingerprint, label: label, token: tok, created: created})
	}
	return res, nil
}

func (l *lxd) query(ctx context.Context, path string, v any) error {
	out, err := l.lxc(ctx, "query", l.o.Remote+":"+path)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), v)
}
