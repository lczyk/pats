package sandbox

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Runs lists everything on the server a pats run made, by its user.pats.run
// token: instances and images in the pats project, bridges and acls in the
// default one, and the run's marker with its lease. the server holds every
// run's record, so every run is judged by it. a cached image (one a
// pats-img-<key> alias names) is not a run's, even though the run that built
// it stamped it: only a run's private alias on it is. an image under a run's
// private alias alone is listed with its last use (see Clean). the shared
// uplink is listed as no run's.
func (l *lxd) Runs(ctx context.Context) ([]RunRecord, error) {
	c := *l
	if c.o.Remote == "" {
		r, err := c.defaultRemote(ctx)
		if err != nil {
			return nil, err
		}
		c.o.Remote = r
	}
	byRun := map[string]*RunRecord{}
	add := func(cfg map[string]string, res Resource) *RunRecord {
		tok := cfg["user.pats.run"]
		if tok == "" {
			return nil
		}
		created, _ := strconv.ParseInt(cfg["user.pats.created"], 10, 64)
		res.Created = time.Unix(created, 0)
		if res.Label == "" {
			res.Label = res.Name
		}
		r := byRun[tok]
		if r == nil {
			r = &RunRecord{Token: tok, Held: true}
			byRun[tok] = r
		}
		if o := cfg["user.pats.owner"]; o != "" {
			r.Owner = o
		}
		r.Resources = append(r.Resources, res)
		return r
	}
	var insts []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := c.query(ctx, "/1.0/instances?project="+lxdProject+"&recursion=1", &insts); err != nil {
		// no pats project: nothing of pats's is there.
		if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
	}
	for _, i := range insts {
		add(i.Config, Resource{Kind: "sandbox", Name: i.Name})
	}
	var nets []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := c.query(ctx, "/1.0/networks?project=default&recursion=1", &nets); err != nil {
		return nil, err
	}
	var shared []Resource
	for _, n := range nets {
		switch {
		case strings.HasPrefix(n.Name, "patsbr"):
			add(n.Config, Resource{Kind: "network", Name: n.Name})
		case n.Name == lxdUplink:
			shared = append(shared, Resource{Kind: "network", Name: n.Name, Label: n.Name + " (the shared uplink)"})
		}
	}
	var acls []struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := c.query(ctx, "/1.0/network-acls?project=default&recursion=1", &acls); err != nil {
		return nil, err
	}
	for _, a := range acls {
		switch {
		case strings.HasPrefix(a.Name, "pats-run-"):
			if r := add(a.Config, Resource{Kind: "marker", Name: a.Name}); r != nil {
				lease, _ := strconv.ParseInt(a.Config["user.pats.lease"], 10, 64)
				r.Lease = time.Unix(lease, 0)
			}
		case strings.HasPrefix(a.Name, "patsbr"):
			add(a.Config, Resource{Kind: "acl", Name: a.Name})
		}
	}
	var imgs []struct {
		Fingerprint string            `json:"fingerprint"`
		Properties  map[string]string `json:"properties"`
		LastUsedAt  time.Time         `json:"last_used_at"`
		UploadedAt  time.Time         `json:"uploaded_at"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := c.query(ctx, "/1.0/images?project="+lxdProject+"&recursion=1", &imgs); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
	}
	for _, im := range imgs {
		tok := im.Properties["user.pats.run"]
		if tok == "" {
			continue
		}
		var private []string
		other := false
		for _, a := range im.Aliases {
			if strings.HasPrefix(a.Name, "pats-img-") && strings.HasSuffix(a.Name, "-"+tok) {
				private = append(private, a.Name)
			} else {
				other = true
			}
		}
		switch {
		case len(private) == 0:
			// claimed, and its alias since re-pointed or removed: init's.
		case other:
			// the claim went through and the private alias's removal did
			// not: the alias alone goes.
			for _, a := range private {
				add(im.Properties, Resource{Kind: "alias", Name: a})
			}
		default:
			// a build that died between publish and claim, or a claimed
			// image a forced init re-pointed away from: nothing tells the
			// two apart, so it goes once unused for a while.
			last := im.LastUsedAt
			if last.Year() <= 1 {
				last = im.UploadedAt
			}
			add(im.Properties, Resource{Kind: "image", Name: im.Fingerprint, Label: private[0], LastUsed: last})
		}
	}
	runs := make([]RunRecord, 0, len(byRun)+1)
	for _, r := range byRun {
		runs = append(runs, *r)
	}
	if len(shared) > 0 {
		runs = append(runs, RunRecord{Held: true, Resources: shared})
	}
	return runs, nil
}

// Remove removes one thing a run left; one already gone is no error.
func (l *lxd) Remove(ctx context.Context, r Resource) error {
	c := *l
	if c.o.Remote == "" {
		remote, err := c.defaultRemote(ctx)
		if err != nil {
			return err
		}
		c.o.Remote = remote
	}
	var err error
	switch r.Kind {
	case "sandbox":
		_, err = c.lxc(ctx, "delete", c.ref(r.Name), "--project", lxdProject, "--force")
	case "network":
		_, err = c.lxc(ctx, "network", "delete", c.ref(r.Name), "--project", "default")
	case "acl", "marker":
		_, err = c.lxc(ctx, "network", "acl", "delete", c.ref(r.Name), "--project", "default")
	case "image":
		_, err = c.lxc(ctx, "image", "delete", c.ref(r.Name), "--project", lxdProject)
	case "alias":
		_, err = c.lxc(ctx, "image", "alias", "delete", c.ref(r.Name), "--project", lxdProject)
	}
	if err != nil && notFound(err) {
		return nil
	}
	return err
}

func (l *lxd) query(ctx context.Context, path string, v any) error {
	out, err := l.lxc(ctx, "query", l.o.Remote+":"+path)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), v)
}
