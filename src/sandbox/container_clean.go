package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Runs lists every container and network carrying a run's label, with its
// creation time: a network's from inspect, since `network ls` has none.
func (c *container) Runs(ctx context.Context) ([]RunRecord, error) {
	byRun := map[string]*RunRecord{}
	add := func(kind, line string) {
		f := strings.SplitN(strings.TrimSpace(line), "|", 4)
		if len(f) < 4 || f[1] == "" {
			return
		}
		name := strings.TrimPrefix(f[0], "/")
		r := byRun[f[1]]
		if r == nil {
			r = &RunRecord{Token: f[1], Owner: f[2]}
			byRun[f[1]] = r
		}
		r.Resources = append(r.Resources, Resource{Kind: kind, Name: name, Label: name, Created: parseCreated(f[3])})
	}
	for _, q := range []struct {
		kind    string
		list    []string
		inspect []string
	}{
		{"sandbox", []string{"ps", "-a", "-q", "--no-trunc", "--filter", "label=pats.run"},
			[]string{"container", "inspect", "--format", `{{.Name}}|{{index .Config.Labels "pats.run"}}|{{index .Config.Labels "pats.owner"}}|{{.Created}}`}},
		{"network", []string{"network", "ls", "-q", "--filter", "label=pats.run"},
			[]string{"network", "inspect", "--format", `{{.Name}}|{{index .Labels "pats.run"}}|{{index .Labels "pats.owner"}}|{{.Created}}`}},
	} {
		out, err := c.dockerOut(ctx, q.list...)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		out, err = c.dockerOut(ctx, append(q.inspect, ids...)...)
		if err != nil && !gone(err) {
			// one went between the list and the inspect: the others are
			// still printed.
			return nil, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			add(q.kind, line)
		}
	}
	runs := make([]RunRecord, 0, len(byRun))
	for _, r := range byRun {
		runs = append(runs, *r)
	}
	return runs, nil
}

// daemonImage is what clean reads of one image on a container daemon.
type daemonImage struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Config      struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	RootFS struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

func (im daemonImage) label() string {
	if len(im.RepoTags) > 0 && !strings.Contains(im.RepoTags[0], "<none>") {
		return im.RepoTags[0]
	}
	hex := strings.TrimPrefix(im.ID, "sha256:")
	return hex[:min(12, len(hex))]
}

// builtOn says whether im may be built on base: another image with base's
// layers, then more or none (a FROM that only sets ENV, CMD or a LABEL adds
// no layer). two with the same layers are each taken as built on the other.
func (im daemonImage) builtOn(base daemonImage) bool {
	a, b := im.RootFS.Layers, base.RootFS.Layers
	return im.ID != base.ID && len(a) >= len(b) && len(b) > 0 && slices.Equal(a[:len(b)], b)
}

// daemonImages reads every image on the daemon, tagged or not.
func (c *container) daemonImages(ctx context.Context) ([]daemonImage, error) {
	out, err := c.dockerOut(ctx, "images", "-a", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := slices.Compact(slices.Sorted(slices.Values(strings.Fields(string(out)))))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = c.dockerOut(ctx, append([]string{"image", "inspect"}, ids...)...)
	if err != nil && !gone(err) {
		return nil, err
	}
	var imgs []daemonImage
	if err := json.Unmarshal(out, &imgs); err != nil {
		return nil, fmt.Errorf("image inspect: %w", err)
	}
	for i := range imgs {
		imgs[i].ID = ImageID(imgs[i].ID)
	}
	return imgs, nil
}

// CachedImages is every image pats made here: those it built, by their
// context hash label, less one that may be built on such an image (the
// label is inherited, so that one is someone's own; of two with the same
// layers neither is taken, and a pin names pats's), and those pulled from
// its registry. ones built on others come first.
func (c *container) CachedImages(ctx context.Context) ([]Resource, error) {
	imgs, err := c.daemonImages(ctx)
	if err != nil {
		return nil, err
	}
	ours := func(im daemonImage) bool {
		for _, r := range append(slices.Clone(im.RepoTags), im.RepoDigests...) {
			if strings.HasPrefix(r, patsRegistry+"/") {
				return true
			}
		}
		h := im.Config.Labels[hashLabel]
		if h == "" {
			return false
		}
		for _, base := range imgs {
			if base.Config.Labels[hashLabel] == h && im.builtOn(base) {
				return false
			}
		}
		return true
	}
	var picked []daemonImage
	for _, im := range imgs {
		if ours(im) {
			picked = append(picked, im)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		if a, b := len(picked[i].RootFS.Layers), len(picked[j].RootFS.Layers); a != b {
			return a > b
		}
		return picked[i].ID < picked[j].ID
	})
	res := make([]Resource, 0, len(picked))
	for _, im := range picked {
		res = append(res, Resource{Kind: "image", Name: im.ID, Label: im.label()})
	}
	return res, nil
}

// ImageChildren is, for every image, the images built on it.
func (c *container) ImageChildren(ctx context.Context) (map[string][]Resource, error) {
	imgs, err := c.daemonImages(ctx)
	if err != nil {
		return nil, err
	}
	kids := map[string][]Resource{}
	for _, base := range imgs {
		for _, im := range imgs {
			if im.builtOn(base) {
				kids[base.ID] = append(kids[base.ID], Resource{Kind: "image", Name: im.ID, Label: im.label()})
			}
		}
	}
	return kids, nil
}

// ImageUsers is every container, running or not, of image id or of one
// built on it.
func (c *container) ImageUsers(ctx context.Context, id string) ([]string, error) {
	out, err := c.dockerOut(ctx, "ps", "-a", "--format", "{{.Names}}", "--filter", "ancestor="+id)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// parseCreated reads a creation time as docker prints it (rfc 3339) or as
// podman does (go's time format); zero when neither.
func parseCreated(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (c *container) Remove(ctx context.Context, r Resource) error {
	var err error
	switch r.Kind {
	case "sandbox":
		_, err = c.dockerOut(ctx, "rm", "-f", r.Name)
	case "network":
		_, err = c.dockerOut(ctx, "network", "rm", r.Name)
	case "image":
		// by its names, so an image under several goes in one call: by id
		// alone the cli refuses it without -f, which would take one in use.
		names := []string{r.Name}
		if out, ierr := c.dockerOut(ctx, "image", "inspect", "--format", `{{join .RepoTags " "}}`, r.Name); ierr == nil {
			if tags := strings.Fields(string(out)); len(tags) > 0 {
				names = tags
			}
		}
		_, err = c.dockerOut(ctx, append([]string{"rmi"}, names...)...)
	default:
		return nil
	}
	if err != nil && gone(err) {
		return nil
	}
	return err
}

// gone says whether a cli's error says the container or network is not
// there: docker's "no such container" and "network x not found", podman's
// "no such container" and "network not found".
func gone(err error) bool {
	return notFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such")
}
