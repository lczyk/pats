package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
)

// how a harness cli gets into an image: fetched as a file checked against a
// sha256, and placed by init (docs/spec/harness-images.md).

// fetchArtefact is the checked fetch; a var so tests fetch nothing.
var fetchArtefact = sandbox.FetchArtefact

// places says whether pats places harness clis in a sandbox's image: one it
// composes, on a driver whose images it builds that way. an image: or a
// build: is the user's whole statement of what is inside, and gets nothing.
func places(sb config.Sandbox) bool {
	if sb.Image != "" || sb.Build != "" {
		return false
	}
	switch sb.Driver {
	case "docker", "podman", sandbox.LXDDriver, "bwrap":
		return true
	}
	return false
}

// artefactsFor is the artefact of each of clis for arch, in their order, by
// the sandbox's pins. a cli with none for the arch is an error naming it,
// and the harness whose own pin it is at.
func artefactsFor(pins map[string]*machine.CLI, clis []string, arch string) ([]sandbox.Artefact, error) {
	var out []sandbox.Artefact
	for _, name := range clis {
		c := pins[name]
		if c == nil {
			return nil, fmt.Errorf("cli %q is not in the clis table", name)
		}
		url, a, ok := c.ArtefactFor(arch, "")
		switch {
		case !ok && c.PinnedBy() != "":
			return nil, fmt.Errorf("harness %s pins cli %s at %s with no hash for %s", c.PinnedBy(), name, c.Version, arch)
		case !ok:
			return nil, fmt.Errorf("cli %s %s has no artefact for %s", name, c.Version, arch)
		case a.Hash == "":
			return nil, fmt.Errorf("cli %s %s has no hash for %s yet: pats harness pin %s %s prints it", name, c.Version, arch, name, c.Version)
		}
		out = append(out, sandbox.Artefact{CLI: name, Version: c.Version, Arch: arch, URL: url, SHA256: a.SHA256(), Bin: a.Bin})
	}
	return out, nil
}

// harnessInput digests what decides the trees an image holds: a line of cli,
// version, arch, sha256 and bin for each, and none of their bytes. a url is
// not in it. "" for no clis.
func harnessInput(arts []sandbox.Artefact) string {
	if len(arts) == 0 {
		return ""
	}
	var lines []string
	for _, a := range arts {
		lines = append(lines, a.Line())
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// harnessInputNow is harnessInput for a sandbox as the config and the tables
// stand, for the arch its image was made for: what a run compares with what
// init recorded, without fetching anything. a cli with no artefact for the
// arch makes it differ from any init's.
func harnessInputNow(cfg *config.Config, sb config.Sandbox, arch string) string {
	if !places(sb) {
		return ""
	}
	clis := sandboxCLIs(cfg, sb.Name)
	arts, err := artefactsFor(sandboxPins(cfg, sb.Name), clis, arch)
	if err != nil {
		return "unplaceable: " + err.Error()
	}
	return harnessInput(arts)
}

// treeIDs is the ids of the trees a pin records, in the order of their clis'
// names: what a place that keeps trees beside its images stacks over one.
func treeIDs(e *pinEntry) []string {
	names := make([]string, 0, len(e.Harnesses))
	for n := range e.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	// an entry with no tree id is kept: no place has a tree of that name,
	// so the pin reads as one to make again.
	var out []string
	for _, n := range names {
		out = append(out, e.Harnesses[n]["tree"])
	}
	return out
}

// fetchPlaced fetches each artefact into the cache, checked, and returns
// what a build places and what the pin records of it.
func fetchPlaced(ctx context.Context, arts []sandbox.Artefact, lg logw, sbName string) ([]sandbox.Placed, map[string]map[string]string, error) {
	var placed []sandbox.Placed
	rec := map[string]map[string]string{}
	for _, a := range arts {
		lg.detail("sandbox %q: %s %s for %s", sbName, a.CLI, a.Version, a.Arch)
		file, err := fetchArtefact(ctx, a, nil)
		if err != nil {
			return nil, nil, err
		}
		placed = append(placed, sandbox.Placed{Artefact: a, File: file})
		rec[a.CLI] = map[string]string{"version": a.Version, "hash": machine.HashKind + a.SHA256, "tree": a.TreeID()}
	}
	return placed, rec, nil
}
