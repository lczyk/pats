// Package machine holds what pats knows about the harnesses it drives: the
// tables of them and of their clis, read from the embedded defaults
// (docs/spec/machine-config.md).
package machine

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
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
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// CLI is one entry of the clis table. its name is the executable's on PATH.
type CLI struct {
	Name      string            `yaml:"-"`
	Version   string            `yaml:"version"`
	LoginFile string            `yaml:"login-file"` // under the home
	HomeEnv   map[string]string `yaml:"home-env"`   // variable -> a path under the harness home
}

// AuthPathVar names the variable that points at a copy of the cli's login
// file: unset, the file is the cli's own under the user's home.
func (c *CLI) AuthPathVar() string {
	return "PATS_" + strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_")) + "_AUTH_PATH"
}

// Harness is one entry of the harnesses table: one way of running an agent.
type Harness struct {
	Name     string   `yaml:"-"`
	CLI      string   `yaml:"cli"`
	Run      string   `yaml:"run"`
	Egress   Egress   `yaml:"egress"`
	AuthEnv  []string `yaml:"auth-env"` // forwarded when set, each a credential
	Login    bool     `yaml:"login"`    // copy the cli's login file into the harness home
	AuthHint string   `yaml:"auth-hint"`
	PassEnv  []string `yaml:"pass-env"` // forwarded when set, not credentials
	Models   string   `yaml:"models"`
	// Parse is the script that reads the command's stdout
	// (docs/spec/harness-parse.md), "" for none.
	Parse string `yaml:"parse"`

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

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Parse reads tables from yaml, refusing unknown keys, and checks them as a
// whole. every error names its entry.
func Parse(data []byte) (*Tables, error) {
	var t Tables
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	return &t, nil
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
		for _, k := range sortedKeys(c.HomeEnv) {
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
