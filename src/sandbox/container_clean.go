package sandbox

import (
	"context"
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
