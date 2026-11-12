package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	return sb.Driver == "docker" || sb.Driver == "podman" || sb.Driver == sandbox.LXDDriver
}

// artefactsFor is the artefact of each of clis for arch, in their order, by
// the tables' pins. a cli with none for the arch is an error naming it.
func artefactsFor(tables *machine.Tables, clis []string, arch string) ([]sandbox.Artefact, error) {
	var out []sandbox.Artefact
	for _, name := range clis {
		c := tables.CLIs[name]
		if c == nil {
			return nil, fmt.Errorf("cli %q is not in the clis table", name)
		}
		url, a, ok := c.ArtefactFor(arch, "")
		if !ok {
			return nil, fmt.Errorf("cli %s %s has no artefact for %s", name, c.Version, arch)
		}
		out = append(out, sandbox.Artefact{CLI: name, Version: c.Version, Arch: arch, URL: url, SHA256: a.SHA256, Bin: a.Bin})
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
	arts, err := artefactsFor(cfg.Tables(), clis, arch)
	if err != nil {
		return "unplaceable: " + err.Error()
	}
	return harnessInput(arts)
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
		rec[a.CLI] = map[string]string{"version": a.Version, "sha256": a.SHA256, "tree": a.TreeID()}
	}
	return placed, rec, nil
}
