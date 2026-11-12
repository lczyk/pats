package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// where an entry's keys were last set.
const (
	FromDefault = "default"
	FromMachine = "machine"
	FromProject = "project"
)

// ConfigPath is where the machine's file is looked for:
// $XDG_CONFIG_HOME/pats/config.yaml when that is set and absolute (a relative
// one is ignored, as the xdg spec says), else ~/.config/pats/config.yaml, on
// every os.
func ConfigPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "pats", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pats", "config.yaml"), nil
}

// Load is the tables pats runs with on this machine: the defaults, with the
// machine's file laid over them when there is one. the file is optional, and
// one that is there and wrong is an error, never half applied.
func Load() (*Tables, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t := Defaults()
		t.Path = path
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("machine config: %w", err)
	}
	t, err := Overlay(data, path)
	if err != nil {
		return nil, err
	}
	t.Found = true
	return t, nil
}

// machineTables are the tables a machine's file may hold: all of them.
var machineTables = tableTypes

// Overlay lays a machine's file over the defaults, entry by entry: an entry
// named as a default changes it key by key, each key replaced whole and a
// null unsetting it, and an entry with a new name is added. the two are laid
// over as written, before anything is decoded. path names the file in errors.
func Overlay(over []byte, path string) (*Tables, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(over, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t := Defaults()
	t.Path, t.lines, t.from = path, map[string]int{}, map[string]string{}
	var errs []string
	add := func(line int, format string, a ...any) {
		errs = append(errs, fmt.Sprintf("%s:%d: %s", path, line, fmt.Sprintf(format, a...)))
	}
	if len(doc.Content) > 0 {
		root := deref(doc.Content[0])
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s:%d: the top level is a mapping of tables", path, root.Line)
		}
		if d := dupKey(root); d != nil {
			add(d.Line, "table %q is there twice", d.Value)
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			table, entries := deref(root.Content[i]), deref(root.Content[i+1])
			typ, ok := machineTables[table.Value]
			switch {
			case !ok:
				add(table.Line, "unknown table %q (clis, harnesses)", table.Value)
				continue
			case isNull(entries):
				continue
			case entries.Kind != yaml.MappingNode:
				add(entries.Line, "%s is a mapping of entries", table.Value)
				continue
			}
			if d := dupKey(entries); d != nil {
				add(d.Line, "%s.%s is there twice", table.Value, d.Value)
			}
			into := mappingValue(t.root, table.Value)
			for j := 0; j+1 < len(entries.Content); j += 2 {
				name, entry := deref(entries.Content[j]), deref(entries.Content[j+1])
				id := table.Value + "." + name.Value
				t.lines[id] = name.Line
				if entry.Kind != yaml.MappingNode {
					add(entry.Line, "%s is a mapping of keys", id)
					continue
				}
				if probs := entryProblems(entry, typ); len(probs) > 0 {
					for _, p := range probs {
						add(p.line, "%s: %s", id, p.msg)
					}
					continue
				}
				// a machine's pin is its clis entry: one more on a harness
				// would be a second place for it.
				if table.Value == "harnesses" {
					pinned := false
					for k := 0; k+1 < len(entry.Content); k += 2 {
						if key := deref(entry.Content[k]); key.Value == "version" || key.Value == "sha256" {
							add(key.Line, "%s: %s is a pats.yaml's key: a machine pins a cli in clis", id, key.Value)
							pinned = true
						}
					}
					if pinned {
						continue
					}
				}
				// an entry that sets nothing changes nothing. where a harness
				// is from is its own entry's: a cli's pin is in its digest.
				if table.Value == "harnesses" && (len(entry.Content) > 0 || mappingValue(into, name.Value) == nil) {
					t.from[name.Value] = FromMachine
				}
				if lay(into, name, entry) {
					t.Adds = append(t.Adds, id)
					continue
				}
				for k := 0; k+1 < len(entry.Content); k += 2 {
					t.Changes = append(t.Changes, id+"."+entry.Content[k].Value)
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	if err := t.build(); err != nil {
		return nil, t.located(err)
	}
	sort.Strings(t.Changes)
	sort.Strings(t.Adds)
	return t, nil
}

// lay puts one entry over the table it belongs to: over the entry of its
// name key by key, each key replaced whole and a null removing it, or as a
// new entry, which it reports.
func lay(table, name, entry *yaml.Node) (added bool) {
	old := mappingValue(table, name.Value)
	if old == nil {
		table.Content = append(table.Content, name, withoutNulls(entry))
		return true
	}
	for k := 0; k+1 < len(entry.Content); k += 2 {
		setKey(old, deref(entry.Content[k]), entry.Content[k+1])
	}
	return false
}

// Item is one harness a pats.yaml defines: its keys as written, a null for
// one it unsets.
type Item struct {
	Name string
	Node *yaml.Node // a mapping
}

// Project lays a pats.yaml's harness items over the tables by the rule the
// machine's file is laid over the defaults by: an item named as a harness
// the machine has changes it key by key, and one with a new name is a
// harness of its own. the tables are checked again as a whole. an error
// names its entry as harnesses.<name>.
func (t *Tables) Project(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	table := mappingValue(t.root, "harnesses")
	if table == nil {
		table = &yaml.Node{Kind: yaml.MappingNode}
		t.root.Content = append(t.root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "harnesses"}, table)
	}
	if t.from == nil {
		t.from = map[string]string{}
	}
	var errs []string
	for _, it := range items {
		for _, p := range entryProblems(it.Node, tableTypes["harnesses"]) {
			errs = append(errs, fmt.Sprintf("line %d: harnesses.%s: %s", p.line, it.Name, p.msg))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	for _, it := range items {
		if lay(table, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: it.Name}, it.Node) || len(it.Node.Content) > 0 {
			t.from[it.Name] = FromProject
		}
	}
	return t.build()
}

// problem is one thing wrong with an entry as written, and the line it is on.
type problem struct {
	line int
	msg  string
}

// entryProblems checks one entry before it is laid over anything: every key
// is one its type takes, nested ones too, and is there once, and its values
// decode. laid over first, a wrong key would be ignored and a second one of
// the same name would win, both without a word.
func entryProblems(entry *yaml.Node, typ reflect.Type) []problem {
	var out []problem
	var walk func(m *yaml.Node, typ reflect.Type, where string)
	walk = func(m *yaml.Node, typ reflect.Type, where string) {
		fields := yamlFields(typ)
		if d := dupKey(m); d != nil {
			out = append(out, problem{d.Line, fmt.Sprintf("%skey %q is there twice", where, d.Value)})
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			key, val := deref(m.Content[i]), deref(m.Content[i+1])
			ft, ok := fields[key.Value]
			switch {
			case !ok:
				out = append(out, problem{key.Line, fmt.Sprintf("%sunknown key %q", where, key.Value)})
			case ft.Kind() == reflect.Struct && val.Kind == yaml.MappingNode:
				walk(val, ft, where+key.Value+": ")
			case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct && val.Kind == yaml.MappingNode:
				// a mapping of entries of its own: each one's keys.
				if d := dupKey(val); d != nil {
					out = append(out, problem{d.Line, fmt.Sprintf("%s%s: %q is there twice", where, key.Value, d.Value)})
				}
				for j := 0; j+1 < len(val.Content); j += 2 {
					if e := deref(val.Content[j+1]); e.Kind == yaml.MappingNode {
						walk(e, ft.Elem(), where+key.Value+" "+deref(val.Content[j]).Value+": ")
					}
				}
			}
		}
	}
	walk(entry, typ, "")
	if len(out) > 0 {
		return out
	}
	err := withoutNulls(entry).Decode(reflect.New(typ).Interface())
	var te *yaml.TypeError
	switch {
	case errors.As(err, &te):
		for _, e := range te.Errors {
			line := entry.Line
			if rest, ok := strings.CutPrefix(e, "line "); ok {
				if n, msg, ok := strings.Cut(rest, ": "); ok {
					if _, serr := fmt.Sscanf(n, "%d", &line); serr == nil {
						e = msg
					}
				}
			}
			out = append(out, problem{line, e})
		}
	case err != nil:
		out = append(out, problem{entry.Line, err.Error()})
	}
	return out
}

// deref is the node an alias stands for, or the node itself.
func deref(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias
	}
	return n
}

// dupKey is the second of two keys of one name in a mapping, nil when each
// is there once.
func dupKey(m *yaml.Node) *yaml.Node {
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := deref(m.Content[i])
		if seen[k.Value] {
			return k
		}
		seen[k.Value] = true
	}
	return nil
}

// located puts the file, and the line of the entry when it is the file's,
// before each of a check's errors.
func (t *Tables) located(err error) error {
	ids := sortedKeys(t.lines)
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) }) // a name another starts with comes later
	var out []string
	for _, msg := range strings.Split(err.Error(), "\n") {
		at := t.Path
		for _, id := range ids {
			if strings.HasPrefix(msg, id+":") || strings.HasPrefix(msg, id+".") {
				at = fmt.Sprintf("%s:%d", t.Path, t.lines[id])
				break
			}
		}
		out = append(out, at+": "+msg)
	}
	return errors.New(strings.Join(out, "\n"))
}

// Note is the one line that says what the machine's file changes and adds,
// "" when it does neither.
func (t *Tables) Note() string {
	if t == nil || len(t.Changes)+len(t.Adds) == 0 {
		return ""
	}
	var parts []string
	if len(t.Changes) > 0 {
		parts = append(parts, "changes "+strings.Join(t.Changes, ", "))
	}
	if len(t.Adds) > 0 {
		parts = append(parts, "adds "+strings.Join(t.Adds, ", "))
	}
	return fmt.Sprintf("machine config %s: %s", t.Path, strings.Join(parts, "; "))
}

// Digest is a sha256 of the resolved entry and of what its cli fixes: the
// definition a run asked for, whatever its name and wherever it was written.
func (h *Harness) Digest() string {
	d := struct {
		CLI      string            `json:"cli"`
		Run      string            `json:"run"`
		Egress   Egress            `json:"egress"`
		AuthEnv  []string          `json:"auth_env"`
		Login    bool              `json:"login"`
		AuthHint string            `json:"auth_hint"`
		PassEnv  []string          `json:"pass_env"`
		Models   string            `json:"models"`
		Parse    string            `json:"parse"`
		Version  string            `json:"version"`
		LoginAt  string            `json:"login_file"`
		HomeEnv  map[string]string `json:"home_env"`
		Files    [][3]string       `json:"artefacts"` // arch, sha256, bin: what a tree holds, not where it is from
	}{CLI: h.CLI, Run: h.Run, Egress: h.Egress, AuthEnv: h.AuthEnv, Login: h.Login, AuthHint: h.AuthHint,
		PassEnv: h.PassEnv, Models: h.Models, Parse: h.Parse}
	if h.cli != nil {
		d.Version, d.LoginAt, d.HomeEnv = h.cli.Version, h.cli.LoginFile, h.cli.HomeEnv
		for _, arch := range Arches {
			if _, a, ok := h.cli.ArtefactFor(arch, ""); ok {
				d.Files = append(d.Files, [3]string{arch, a.SHA256, a.Bin})
			}
		}
	}
	b, err := json.Marshal(d) // map keys are sorted
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isNull(n *yaml.Node) bool {
	n = deref(n)
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// mappingValue is the value of key in a mapping node, nil when it has none.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if deref(m.Content[i]).Value == key {
			return deref(m.Content[i+1])
		}
	}
	return nil
}

// setKey replaces key's whole value in a mapping, or with a null removes the
// key: absent means keep, and a null means unset.
func setKey(m, key, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key.Value {
			continue
		}
		if isNull(val) {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		} else {
			m.Content[i+1] = val
		}
		return
	}
	if !isNull(val) {
		m.Content = append(m.Content, key, val)
	}
}

// withoutNulls is a new entry's mapping less the keys it nulls.
func withoutNulls(m *yaml.Node) *yaml.Node {
	out := *m
	out.Content = nil
	for i := 0; i+1 < len(m.Content); i += 2 {
		if !isNull(m.Content[i+1]) {
			out.Content = append(out.Content, m.Content[i], m.Content[i+1])
		}
	}
	return &out
}

// yamlFields is the keys a struct takes from yaml, and each one's type.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			fields[name] = t.Field(i).Type
		}
	}
	return fields
}

// yamlKeys is the keys a struct takes from yaml.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for k := range yamlFields(t) {
		keys[k] = true
	}
	return keys
}

// DefaultsYAML is the defaults as they are written, comments and all: the
// text to copy an entry from.
func DefaultsYAML() []byte { return defaultsYAML }

// Print writes the tables as yaml, under a comment that names the machine's
// file and says whether it is there. tables the file does not touch are the
// defaults as they are written.
func (t *Tables) Print(w io.Writer) error {
	switch {
	case !t.Found:
		fmt.Fprintf(w, "# machine config: %s (not there: these are the defaults)\n", t.Path)
	case len(t.Changes)+len(t.Adds) == 0:
		fmt.Fprintf(w, "# machine config: %s (changes nothing: these are the defaults)\n", t.Path)
	default:
		fmt.Fprintf(w, "# %s\n", t.Note())
	}
	if len(t.Changes)+len(t.Adds) == 0 {
		_, err := w.Write(defaultsYAML)
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(t); err != nil {
		return err
	}
	return enc.Close()
}
