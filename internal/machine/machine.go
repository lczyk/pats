// Package machine holds what pats knows about the harnesses it drives: the
// tables of them and of their clis, read from the embedded defaults
// (docs/spec/machine-config.md).
package machine

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lczyk/pats/internal/parse"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

//go:embed defaults.yaml
var defaultsYAML []byte

// Egress is a harness's part of a pair's policy, in a task's shape.
type Egress struct {
	Allow []string `yaml:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty"`
}

// CLI is one entry of the clis table. its name is the executable's on PATH.
type CLI struct {
	Name      string            `yaml:"-"`
	Version   string            `yaml:"version"`
	LoginFile string            `yaml:"login-file,omitempty"` // under the home
	HomeEnv   map[string]string `yaml:"home-env,omitempty"`   // variable -> a path under the harness home
	// Artefacts are the files the cli is published as, each keyed by its
	// url, with ${version} for the version.
	Artefacts map[string]Artefact `yaml:"artefacts,omitempty"`
}

// Artefact is one file a cli is published as (docs/spec/harness-images.md).
type Artefact struct {
	Arch   []string `yaml:"arch"`          // the arches the file is for
	SHA256 string   `yaml:"sha256"`        // the file's
	Bin    string   `yaml:"bin,omitempty"` // the executable inside the file, a gzipped tar; "" when the file is the executable
}

// Arches are the arches a harness cli is placed for.
var Arches = []string{"amd64", "arm64"}

// ArtefactKey is the cli's artefact for arch under the url it is written
// with, ${version} and all. ok is false when it has none for the arch.
func (c *CLI) ArtefactKey(arch string) (key string, a Artefact, ok bool) {
	for _, u := range sortedKeys(c.Artefacts) {
		if a := c.Artefacts[u]; slices.Contains(a.Arch, arch) {
			return u, a, true
		}
	}
	return "", Artefact{}, false
}

// ArtefactFor is the cli's artefact for arch, with its url at version (the
// cli's own when version is "").
func (c *CLI) ArtefactFor(arch, version string) (url string, a Artefact, ok bool) {
	if version == "" {
		version = c.Version
	}
	key, a, ok := c.ArtefactKey(arch)
	return strings.ReplaceAll(key, "${version}", version), a, ok
}

// AuthPathVar names the variable that points at a copy of the cli's login
// file: unset, the file is the cli's own under the user's home.
func (c *CLI) AuthPathVar() string {
	return "PATS_" + strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_")) + "_AUTH_PATH"
}

// Harness is one entry of the harnesses table: one way of running an agent.
type Harness struct {
	Name     string   `yaml:"-"`
	CLI      string   `yaml:"cli,omitempty"`
	Run      string   `yaml:"run"`
	Egress   Egress   `yaml:"egress,omitempty"`
	AuthEnv  []string `yaml:"auth-env,omitempty"` // forwarded when set, each a credential
	Login    bool     `yaml:"login,omitempty"`    // copy the cli's login file into the harness home
	AuthHint string   `yaml:"auth-hint,omitempty"`
	PassEnv  []string `yaml:"pass-env,omitempty"` // forwarded when set, not credentials
	Models   string   `yaml:"models,omitempty"`
	// Parse is the script that reads the command's stdout
	// (docs/spec/harness-parse.md), "" for none.
	Parse string `yaml:"parse,omitempty"`

	// From is the last place that set a key of the entry: FromDefault,
	// FromMachine or FromProject.
	From string `yaml:"-"`

	cli    *CLI
	parser *parse.Program
}

// Parser is the harness's compiled parse script, nil for a harness with none.
func (h *Harness) Parser() *parse.Program {
	if h == nil {
		return nil
	}
	return h.parser
}

// Cli is the harness's cli entry, nil for a harness with none.
func (h *Harness) Cli() *CLI { return h.cli }

// LoginFile is the path under the home of the login file the harness wants
// copied in, "" for none.
func (h *Harness) LoginFile() string {
	if !h.Login || h.cli == nil {
		return ""
	}
	return h.cli.LoginFile
}

// Catalogues are the model catalogues an entry's `models` may name.
var Catalogues = map[string]bool{"openrouter": true}

// Tables are the clis and harnesses pats runs with.
type Tables struct {
	CLIs      map[string]*CLI     `yaml:"clis"`
	Harnesses map[string]*Harness `yaml:"harnesses"`

	// Path is where the machine's file is looked for, and Found whether it
	// is there. Changes and Adds are what it does to the defaults, as dotted
	// keys (harnesses.<name>.<key>) and entries (harnesses.<name>).
	Path    string   `yaml:"-"`
	Found   bool     `yaml:"-"`
	Changes []string `yaml:"-"`
	Adds    []string `yaml:"-"`

	root  *yaml.Node        // the tables as written: what an overlay is laid over
	lines map[string]int    // the line of each entry of the machine's file
	from  map[string]string // harness -> the last place that set a key of it
}

// Harness is the entry named name, nil when there is none.
func (t *Tables) Harness(name string) *Harness {
	if t == nil {
		return nil
	}
	return t.Harnesses[name]
}

// Names lists the harnesses, sorted.
func (t *Tables) Names() []string {
	out := make([]string, 0, len(t.Harnesses))
	for n := range t.Harnesses {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Defaults parses the embedded defaults afresh: the caller owns the result.
func Defaults() *Tables {
	t, err := Parse(defaultsYAML)
	if err != nil {
		panic("machine: embedded defaults: " + err.Error())
	}
	return t
}

// tableTypes are the tables a file of this format holds, and an entry's type.
var tableTypes = map[string]reflect.Type{
	"clis":      reflect.TypeOf(CLI{}),
	"harnesses": reflect.TypeOf(Harness{}),
}

// HarnessKeys are the keys a harness entry takes.
func HarnessKeys() map[string]bool { return yamlKeys(tableTypes["harnesses"]) }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Parse reads tables from yaml, refusing unknown keys, and checks them as a
// whole. every error names its entry.
func Parse(data []byte) (*Tables, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := &yaml.Node{Kind: yaml.MappingNode}
	if len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: the top level is a mapping of tables", root.Line)
	}
	var errs []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		table, entries := root.Content[i], root.Content[i+1]
		typ, ok := tableTypes[table.Value]
		if !ok {
			errs = append(errs, fmt.Sprintf("line %d: unknown table %q (clis, harnesses)", table.Line, table.Value))
			continue
		}
		if entries.Kind != yaml.MappingNode {
			continue // decoding says what is wrong with it
		}
		for j := 0; j+1 < len(entries.Content); j += 2 {
			name, entry := entries.Content[j], deref(entries.Content[j+1])
			if entry.Kind != yaml.MappingNode {
				continue
			}
			for _, p := range entryProblems(entry, typ) {
				errs = append(errs, fmt.Sprintf("line %d: %s.%s: %s", p.line, table.Value, name.Value, p.msg))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	t := &Tables{root: root}
	if err := t.build(); err != nil {
		return nil, err
	}
	return t, nil
}

// build decodes the tables from their yaml and checks them as a whole.
func (t *Tables) build() error {
	t.CLIs, t.Harnesses = nil, nil
	if err := t.root.Decode(t); err != nil {
		return err
	}
	return t.check()
}

// check validates the tables and resolves each harness's cli.
func (t *Tables) check() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	for _, name := range sortedKeys(t.CLIs) {
		c := t.CLIs[name]
		if c == nil {
			c = &CLI{}
			t.CLIs[name] = c
		}
		c.Name = name
		if !nameRe.MatchString(name) {
			add("clis.%s: a name is lower-case letters, digits and -", name)
		}
		if c.Version == "" {
			add("clis.%s: version is required", name)
		}
		if c.LoginFile != "" && !underHome(c.LoginFile) {
			add("clis.%s: login-file %q is not a path under the home", name, c.LoginFile)
		}
		if len(c.Artefacts) == 0 {
			add("clis.%s: at least one artefact is required", name)
		}
		taken := map[string]string{}
		for _, u := range sortedKeys(c.Artefacts) {
			a := c.Artefacts[u]
			if parsed, err := url.Parse(u); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				add("clis.%s: artefact %q is not an http or https url", name, u)
			}
			// a url with no version in it serves the same bytes for every pin.
			if !strings.Contains(u, "${version}") {
				add("clis.%s: artefact %s: the url has no ${version}", name, u)
			}
			if len(a.Arch) == 0 {
				add("clis.%s: artefact %s: arch is required (%s)", name, u, strings.Join(Arches, ", "))
			}
			for _, x := range a.Arch {
				switch {
				case !slices.Contains(Arches, x):
					add("clis.%s: artefact %s: arch %q is not one of %s", name, u, x, strings.Join(Arches, ", "))
				case taken[x] != "":
					add("clis.%s: two artefacts are for %s: %s and %s", name, x, taken[x], u)
				}
				taken[x] = u
			}
			if !sha256Re.MatchString(a.SHA256) {
				add("clis.%s: artefact %s: sha256 is 64 lower-case hex digits", name, u)
			}
			if a.Bin != "" && !underHome(a.Bin) {
				add("clis.%s: artefact %s: bin %q is not a path inside the artefact", name, u, a.Bin)
			}
		}
		for _, k := range sortedKeys(c.HomeEnv) {
			if why := badEnv(k); why != "" {
				add("clis.%s: home-env %s %s", name, k, why)
			}
			if !underHome(c.HomeEnv[k]) {
				add("clis.%s: home-env %s: %q is not a path under the home", name, k, c.HomeEnv[k])
			}
		}
	}
	for _, name := range sortedKeys(t.Harnesses) {
		h := t.Harnesses[name]
		if h == nil {
			h = &Harness{}
			t.Harnesses[name] = h
		}
		h.Name = name
		if h.From = t.from[name]; h.From == "" {
			h.From = FromDefault
		}
		if !nameRe.MatchString(name) {
			add("harnesses.%s: a name is lower-case letters, digits and -", name)
		}
		if strings.TrimSpace(h.Run) == "" {
			add("harnesses.%s: run is required", name)
		}
		h.cli = nil
		if h.CLI != "" {
			if h.cli = t.CLIs[h.CLI]; h.cli == nil {
				add("harnesses.%s: cli %q is not in clis", name, h.CLI)
			}
		}
		if h.Login && (h.cli == nil || h.cli.LoginFile == "") {
			add("harnesses.%s: login needs a cli with a login-file", name)
		}
		// a variable has one source: named twice, which value the sandbox
		// gets would depend on the order pats happens to set them in.
		seen := map[string]string{}
		if h.cli != nil {
			for k := range h.cli.HomeEnv {
				seen[k] = "its cli's home-env"
			}
		}
		for _, list := range []struct {
			key   string
			names []string
		}{{"auth-env", h.AuthEnv}, {"pass-env", h.PassEnv}} {
			for _, k := range list.names {
				if why := badEnv(k); why != "" {
					add("harnesses.%s: %s %s %s", name, list.key, k, why)
				} else if where, dup := seen[k]; dup {
					add("harnesses.%s: %s %s is also in %s", name, list.key, k, where)
				}
				seen[k] = list.key
			}
		}
		if h.Models != "" && !Catalogues[h.Models] {
			add("harnesses.%s: models %q is not a catalogue pats has (%s)", name, h.Models, strings.Join(sortedKeys(Catalogues), ", "))
		}
		h.parser = nil
		if strings.TrimSpace(h.Parse) != "" {
			var err error
			if h.parser, err = parse.Compile("harnesses."+name+".parse", h.Parse); err != nil {
				add("%v", err)
			}
		}
		for _, list := range []struct {
			key     string
			entries []string
		}{{"allow", h.Egress.Allow}, {"deny", h.Egress.Deny}} {
			for _, line := range list.entries {
				e, err := proxy.ParseEntry(line, list.key == "deny")
				switch {
				case err != nil:
					add("harnesses.%s: egress %s %q: %v", name, list.key, line, err)
				case e.URL() || e.Methods() != nil:
					add("harnesses.%s: egress %s %q: a harness takes hosts only (a name, a .suffix, a host:port)", name, list.key, line)
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

var envRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// badEnv says why an entry may not name variable k, "" when it may.
func badEnv(k string) string {
	switch {
	case !envRe.MatchString(k):
		return "is not a variable's name"
	case k == "HOME" || strings.HasPrefix(k, "PATS_"):
		return "is pats's own to set"
	}
	return ""
}

// underHome is whether p is a relative path that stays inside its base.
func underHome(p string) bool {
	return p != "" && filepath.IsLocal(filepath.FromSlash(p))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
