package eval

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lczyk/pats/src/sandbox"
)

// lxdUnusedFor is how long an lxd image must go unused before init removes
// it: a server is shared across machines, whose pins no list can reach.
const lxdUnusedFor = 30 * 24 * time.Hour

// remover is a host init may remove superseded images from.
type remover interface {
	// RemoveImage removes image id; an image still in use says so in its
	// error, one already gone is no error.
	RemoveImage(ctx context.Context, id string) error
}

// hostForTarget is the host a pin file target names, as it answers now;
// a var so tests fake it. ok is false for a target this pats cannot reach
// as named: another context, connection or daemon answering now.
var hostForTarget = func(ctx context.Context, target, daemon string) (h imageHost, ok bool) {
	kind, rest, _ := strings.Cut(target, ":")
	switch kind {
	case "lxd":
		d, err := sandbox.Open(sandbox.Options{Driver: "lxd", Remote: rest}.WithEnv())
		return d, err == nil
	case "docker", "podman": // sandbox.Engines()
		d, err := sandbox.Open(sandbox.Options{Driver: kind}.WithEnv())
		if err != nil {
			return nil, false
		}
		now, err := d.Target(ctx)
		return d, err == nil && now.Key == target && now.Daemon == daemon
	}
	return nil, false
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

// machineLock serialises what inits on this machine do with each other's
// pins: the projects file, a commit of new pins, and the removal that reads
// every project's pins. it does not cover building, which can take minutes.
func machineLock() (func(), error) {
	path := projectsFile() + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// registerProject adds configDir to the projects file, drops listed paths
// that are gone, and returns the list. the caller holds machineLock.
func registerProject(configDir string) ([]string, error) {
	path := projectsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p := strings.TrimSpace(sc.Text())
			if p == "" || seen[p] {
				continue
			}
			// only a project that is surely gone is forgotten: one this
			// process cannot look at now may still pin images.
			if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
		err := sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
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

	mine := f.pinnedIDs()
	var keep []superseded
	for _, s := range f.Superseded {
		why := ""
		h, ok := hostForTarget(ctx, s.Target, s.Daemon)
		switch {
		case f.claim(s):
			continue // pinned again here, and pats's own there now
		case mine[s.ID]:
			// two targets may reach one daemon or server.
			why = "this project still pins it under another target"
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
	if l, ok := h.(sandbox.ImageUsage); ok {
		u, err := l.Usage(ctx, s.ID)
		if err != nil {
			if errors.Is(err, sandbox.ErrNoImage) {
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
	if err := r.RemoveImage(ctx, s.ID); err != nil {
		return err.Error()
	}
	return ""
}
