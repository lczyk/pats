package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// pinsVersion is the pin file's own format.
const pinsVersion = 1

// pinsName is the pin file under .pats/: what `pats init` made for each
// sandbox, by target, and what `pats run` starts from.
const pinsName = "images.json"

// pinImage is one pinned image. ID is what a run starts from: an image id or
// an lxd fingerprint. Built or Pulled says pats made it here, which is what
// lets a later init remove it once nothing pins it.
type pinImage struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest,omitempty"`
	Built  bool   `json:"built,omitempty"`
	Pulled bool   `json:"pulled,omitempty"`
}

// owned says whether pats made the image, so may remove it.
func (p pinImage) owned() bool { return p.Built || p.Pulled }

// pinEntry is one sandbox's pin under a target.
type pinEntry struct {
	Daemon      string            `json:"daemon,omitempty"`
	Image       pinImage          `json:"image"`
	Layers      []string          `json:"layers,omitempty"`
	Proxy       *pinImage         `json:"proxy,omitempty"`
	PatsVersion string            `json:"pats_version"`
	PatsBuild   string            `json:"pats_build"`
	Built       string            `json:"built"`
	Inputs      map[string]string `json:"inputs"`
	Components  map[string]string `json:"components,omitempty"`
}

// images is every image the entry pins: the image, its layers and the proxy.
func (e *pinEntry) images() []pinImage {
	out := []pinImage{e.Image}
	for _, l := range e.Layers {
		out = append(out, pinImage{ID: l, Built: true})
	}
	if e.Proxy != nil {
		out = append(out, *e.Proxy)
	}
	return out
}

// superseded is an image an init moved a pin off, kept until a later init
// can remove it.
type superseded struct {
	Target string    `json:"target"`
	Daemon string    `json:"daemon,omitempty"`
	ID     string    `json:"id"`
	Name   string    `json:"name,omitempty"`
	Built  bool      `json:"built,omitempty"`
	Pulled bool      `json:"pulled,omitempty"`
	Since  time.Time `json:"since"`
}

// pinFile is .pats/images.json: a version, the superseded list, and one
// object per target, sandbox name -> entry.
type pinFile struct {
	Version    int
	Superseded []superseded
	Targets    map[string]map[string]*pinEntry
}

func (f *pinFile) MarshalJSON() ([]byte, error) {
	m := map[string]any{"version": f.Version}
	if len(f.Superseded) > 0 {
		m["superseded"] = f.Superseded
	}
	for t, entries := range f.Targets {
		if len(entries) > 0 {
			m[t] = entries
		}
	}
	return json.Marshal(m)
}

func (f *pinFile) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.Targets = map[string]map[string]*pinEntry{}
	for k, v := range raw {
		var err error
		switch k {
		case "version":
			err = json.Unmarshal(v, &f.Version)
		case "superseded":
			err = json.Unmarshal(v, &f.Superseded)
		default:
			var entries map[string]*pinEntry
			err = json.Unmarshal(v, &entries)
			f.Targets[k] = entries
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// entry is the pin of sandbox sb under target, or nil.
func (f *pinFile) entry(target, sb string) *pinEntry {
	return f.Targets[target][sb]
}

// readPins reads the pin file; a missing one is an empty file.
func readPins(configDir string) (*pinFile, error) {
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	b, err := os.ReadFile(filepath.Join(configDir, ".pats", pinsName))
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf(".pats/%s: %w (run `pats init` to make it again)", pinsName, err)
	}
	if f.Version != pinsVersion {
		return nil, fmt.Errorf(".pats/%s is format %d, this pats reads %d: run `pats init`", pinsName, f.Version, pinsVersion)
	}
	f.normalizeIDs()
	return f, nil
}

// normalizeIDs spells every container image id as the driver does now
// (sandbox.ImageID): a pin podman's inspect wrote holds the hex alone, and
// read as another id it would supersede, and so remove, its own image.
func (f *pinFile) normalizeIDs() {
	container := func(target string) bool {
		kind, _, _ := strings.Cut(target, ":")
		return sandbox.IsEngine(kind)
	}
	for target, entries := range f.Targets {
		if !container(target) {
			continue
		}
		for _, e := range entries {
			e.Image.ID = sandbox.ImageID(e.Image.ID)
			if e.Proxy != nil {
				e.Proxy.ID = sandbox.ImageID(e.Proxy.ID)
			}
		}
	}
	for i, s := range f.Superseded {
		if container(s.Target) {
			f.Superseded[i].ID = sandbox.ImageID(s.ID)
		}
	}
}

// updatePins applies change to the pin file under .pats/images.lock: it
// re-reads the file, so a concurrent init's entries survive, and replaces it
// by rename, so a reader never sees half of it.
func updatePins(configDir string, change func(*pinFile) error) error {
	dir := filepath.Join(configDir, ".pats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "images.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	f, err := readPins(configDir)
	if err != nil {
		// a pin file this pats cannot read is replaced: init remakes what it needs.
		f = &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	}
	if err := change(f); err != nil {
		return err
	}
	f.Version = pinsVersion
	sort.Slice(f.Superseded, func(i, j int) bool { return f.Superseded[i].Since.Before(f.Superseded[j].Since) })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, pinsName+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, pinsName))
}

// setPin pins entry for sb under target, and puts the images the old entry
// pinned and the new one does not on the superseded list.
func (f *pinFile) setPin(target, sb string, e *pinEntry, now time.Time) {
	if f.Targets[target] == nil {
		f.Targets[target] = map[string]*pinEntry{}
	}
	if old := f.Targets[target][sb]; old != nil {
		if old.Daemon == e.Daemon {
			e.inheritOwnership(old)
		}
		f.supersede(target, old, e, now)
	}
	f.reclaim(target, e)
	f.Targets[target][sb] = e
}

// claim hands s's ownership to every pin of this file that pins s's image
// on s's target, and says whether there was one.
func (f *pinFile) claim(s superseded) bool {
	claimed := false
	for _, e := range f.Targets[s.Target] {
		if e.Daemon != s.Daemon {
			continue // the same name on another daemon is another image
		}
		take := func(im *pinImage) {
			if im.ID == s.ID {
				im.Built, im.Pulled = im.Built || s.Built, im.Pulled || s.Pulled
				claimed = true
			}
		}
		take(&e.Image)
		if e.Proxy != nil {
			take(e.Proxy)
		}
		for _, l := range e.Layers {
			claimed = claimed || l == s.ID
		}
	}
	return claimed
}

// reclaim takes off the superseded list the images a new pin pins again,
// keeping them pats's own: an image pats pulled for one sandbox and another
// sandbox now pins as found is still pats's to remove later.
func (f *pinFile) reclaim(target string, e *pinEntry) {
	var keep []superseded
	for _, s := range f.Superseded {
		claimed := false
		take := func(im *pinImage) {
			if s.Target == target && s.Daemon == e.Daemon && im.ID == s.ID {
				im.Built, im.Pulled = im.Built || s.Built, im.Pulled || s.Pulled
				claimed = true
			}
		}
		take(&e.Image)
		if e.Proxy != nil {
			take(e.Proxy)
		}
		for _, l := range e.Layers {
			if s.Target == target && s.Daemon == e.Daemon && l == s.ID {
				claimed = true
			}
		}
		if !claimed {
			keep = append(keep, s)
		}
	}
	f.Superseded = keep
}

// inheritOwnership keeps what pats made as pats's when a new pin keeps the
// same image: an init that finds its earlier pull present pulls nothing, but
// the image is still the one pats brought.
func (e *pinEntry) inheritOwnership(old *pinEntry) {
	owned := map[string]pinImage{}
	for _, im := range old.images() {
		if im.owned() {
			owned[im.ID] = im
		}
	}
	keep := func(im *pinImage) {
		if o, ok := owned[im.ID]; ok {
			im.Built = im.Built || o.Built
			im.Pulled = im.Pulled || o.Pulled
		}
	}
	keep(&e.Image)
	if e.Proxy != nil {
		keep(e.Proxy)
	}
}

// dropMissing removes the entries of sandboxes not in keep, superseding what
// they pinned.
func (f *pinFile) dropMissing(keep map[string]bool, now time.Time) {
	for target, entries := range f.Targets {
		for sb, e := range entries {
			if !keep[sb] {
				f.supersede(target, e, nil, now)
				delete(entries, sb)
			}
		}
	}
}

// supersede lists the images old pinned and next does not (next nil: none),
// those pats made, once each.
func (f *pinFile) supersede(target string, old, next *pinEntry, now time.Time) {
	kept := map[string]bool{}
	if next != nil && next.Daemon == old.Daemon {
		for _, im := range next.images() {
			kept[im.ID] = true
		}
	}
	key := func(target, daemon, id string) string { return target + "\x00" + daemon + "\x00" + id }
	listed := map[string]bool{}
	for _, s := range f.Superseded {
		listed[key(s.Target, s.Daemon, s.ID)] = true
	}
	for _, im := range old.images() {
		k := key(target, old.Daemon, im.ID)
		if !im.owned() || im.ID == "" || kept[im.ID] || listed[k] {
			continue
		}
		listed[k] = true
		f.Superseded = append(f.Superseded, superseded{Target: target, Daemon: old.Daemon, ID: im.ID, Name: im.Name,
			Built: im.Built, Pulled: im.Pulled, Since: now.UTC()})
	}
}

// patsBuild is "release" for a tagged install of a release, else the build's
// commit, -dirty when its tree was; "dev" when it is unknown.
func patsBuild() string {
	i := version.Info
	if i.CommitSHA == "" {
		if sandbox.IsRelease(i.Version) {
			return "release"
		}
		return "dev"
	}
	if i.BuildInfo == "dirty" {
		return i.CommitSHA + "-dirty"
	}
	return i.CommitSHA
}
