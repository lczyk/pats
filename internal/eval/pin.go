package eval

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
)

// PinCLI fetches a cli's artefacts at a version and prints the pin they
// make: as a clis entry, and as the keys of a pats.yaml's harness item. what
// it prints is what the source served just now, and it says so
// (docs/spec/harness-images.md).
func PinCLI(ctx context.Context, tables *machine.Tables, cli, version string, client *http.Client, out io.Writer) error {
	c := tables.CLIs[cli]
	if c == nil {
		names := make([]string, 0, len(tables.CLIs))
		for n := range tables.CLIs {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown cli %q (%s)", cli, strings.Join(names, ", "))
	}
	// an artefact for several arches is one file, fetched once.
	byKey := map[string]string{}
	shas := map[string]string{}
	var keys []string
	for _, arch := range machine.Arches {
		key, a, ok := c.ArtefactKey(arch)
		if !ok {
			return fmt.Errorf("cli %q has no artefact for %s", cli, arch)
		}
		if _, done := byKey[key]; !done {
			url, _, _ := c.ArtefactFor(arch, version)
			sum, err := sandbox.PinArtefact(ctx, sandbox.Artefact{CLI: cli, Version: version, Arch: strings.Join(a.Arch, "+"), URL: url, Bin: a.Bin}, client)
			if err != nil {
				return err
			}
			byKey[key] = sum
			keys = append(keys, key)
		}
		shas[arch] = byKey[key]
	}

	fmt.Fprintf(out, "# %s %s, as its source served it just now: trust on first use. check each hash against the vendor's\n", cli, version)
	fmt.Fprintf(out, "# own where there is one (npm: the package's dist.integrity; a github release: its digests).\n\n")
	fmt.Fprintf(out, "# a clis entry, for a machine's config.yaml or the defaults:\nclis:\n  %s:\n    version: %s\n    artefacts:\n", cli, version)
	for _, key := range keys {
		a := c.Artefacts[key]
		fmt.Fprintf(out, "      %s:\n        arch: [%s]\n        hash: sha256:%s\n", key, strings.Join(a.Arch, ", "), byKey[key])
		if a.Bin != "" {
			fmt.Fprintf(out, "        bin: %s\n", a.Bin)
		}
	}
	fmt.Fprintf(out, "\n# the pin of a harness item in a pats.yaml:\n    version: %s\n    hash:\n", version)
	for _, arch := range machine.Arches {
		fmt.Fprintf(out, "      %s: sha256:%s\n", arch, shas[arch])
	}
	return nil
}
