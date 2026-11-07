package eval

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/src/sandbox"
)

// lxdUnusedFor is how long an lxd image must go unused before init removes
// it: a server is shared across machines, whose pins no list can reach.
const lxdUnusedFor = 30 * 24 * time.Hour

// remover is a host init may remove superseded images from.
type remover interface {
	// remove removes image id; an image still in use or already gone says
	// so in its error, or none for gone.
	remove(ctx context.Context, id string) error
}

// lxdRemover is an lxd server's side of removal.
type lxdRemover interface {
	remover
	usage(ctx context.Context, id string) (sandbox.LXDImageUse, error)
}

// hostForTarget is the host a pin file target names, as it answers now;
// a var so tests fake it. ok is false for a target this pats cannot reach
// as named: another context, connection or daemon answering now.
var hostForTarget = func(ctx context.Context, target, daemon string) (h imageHost, ok bool) {
	kind, rest, _ := strings.Cut(target, ":")
	switch kind {
	case "lxd":
		return lxdHost{o: sandbox.Options{Driver: "lxd", Remote: rest}.WithEnv()}, true
	case "docker", "podman":
		h := containerHost{driver: kind}
		now, d, err := h.target(ctx)
		return h, err == nil && now == target && d == daemon
	}
	return nil, false
}

func (h containerHost) remove(ctx context.Context, id string) error {
	if !h.present(ctx, id) {
		return nil
	}
	_, err := h.cli(ctx, "rmi", id) // no -f: an image a container uses stays
	return err
}

func (h lxdHost) remove(ctx context.Context, id string) error {
	return sandbox.LXDDeleteImage(ctx, h.o, id)
}

func (h lxdHost) usage(ctx context.Context, id string) (sandbox.LXDImageUse, error) {
	return sandbox.LXDImageUsage(ctx, h.o, id)
}

// projectsFile lists every project whose init ran on this machine, one path
// a line, so a container image goes only when none of their pins names it.
func projectsFile() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "pats", "projects")
}

// registerProject adds configDir to the projects file, drops listed paths
// that are gone, and returns the list.
func registerProject(configDir string) ([]string, error) {
	path := projectsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p := strings.TrimSpace(sc.Text())
			if p == "" || seen[p] {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
		f.Close()
	}
	if !seen[configDir] {
		out = append(out, configDir)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "projects.*")
	if err != nil {
		return nil, err
	}
	if _, err := tmp.WriteString(strings.Join(out, "\n") + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	tmp.Close()
	return out, os.Rename(tmp.Name(), path)
}

// pinnedIDs is every image id a pin file pins, on any target.
func (f *pinFile) pinnedIDs() map[string]bool {
	ids := map[string]bool{}
	for _, entries := range f.Targets {
		for _, e := range entries {
			for _, im := range e.images() {
				ids[im.ID] = true
			}
		}
	}
	return ids
}

// removeSuperseded tries every image on f's superseded list, on every
// target, and keeps the ones that may not go yet. f is this project's pin
// file, already holding this init's pins; projects are the registered ones.
// it logs what went and what stayed, and why.
func removeSuperseded(ctx context.Context, f *pinFile, configDir string, projects []string, now time.Time, lg logw) {
	if len(f.Superseded) == 0 {
		return
	}
	mine := f.pinnedIDs()
	// every listed project's pins, for container images; nil when one
	// cannot be read, and then no container image goes.
	others := map[string]bool{}
	for _, p := range projects {
		if p == configDir {
			continue
		}
		pf, err := readPins(p)
		if err != nil {
			lg.warn("keeping superseded container images: the pins of %s cannot be read (%v)", p, err)
			others = nil
			break
		}
		for id := range pf.pinnedIDs() {
			others[id] = true
		}
	}

	var keep []superseded
	for _, s := range f.Superseded {
		why := ""
		h, ok := hostForTarget(ctx, s.Target, s.Daemon)
		switch {
		case mine[s.ID]:
			continue // pinned again: not superseded any more
		case !ok:
			why = "its " + s.Target + " is not the one answering now"
		default:
			why = removeOne(ctx, h, s, others, now)
		}
		if why == "" {
			lg.info("removed superseded image %s (%s)", short(s.ID), s.Target)
			continue
		}
		lg.info("kept superseded image %s (%s): %s", short(s.ID), s.Target, why)
		keep = append(keep, s)
	}
	f.Superseded = keep
}

// removeOne removes one superseded image, or says why it stays.
func removeOne(ctx context.Context, h imageHost, s superseded, others map[string]bool, now time.Time) string {
	r, ok := h.(remover)
	if !ok {
		return "its host cannot remove images"
	}
	if l, ok := h.(lxdRemover); ok {
		u, err := l.usage(ctx, s.ID)
		if err != nil {
			if !h.present(ctx, s.ID) {
				return "" // gone already
			}
			return err.Error()
		}
		last := u.LastUsed
		if last.IsZero() {
			last = u.Uploaded
		}
		switch {
		case u.RunPrivate:
			return "a run holds it under its private alias"
		case now.Sub(last) < lxdUnusedFor:
			return fmt.Sprintf("used %s ago, may serve another machine", now.Sub(last).Round(time.Hour))
		}
	} else {
		switch {
		case others == nil:
			return "another project's pins cannot be read"
		case others[s.ID]:
			return "another project on this machine pins it"
		}
	}
	if err := r.remove(ctx, s.ID); err != nil {
		return err.Error()
	}
	return ""
}
