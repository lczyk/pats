package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a pats.yaml body whose `dir` vectors resolve against the
// current directory. see ParseIn.
func Parse(data []byte) (*Config, error) { return ParseIn(".", data) }

// ParseIn decodes a pats.yaml body into a Config: every vector's items merged
// with its defaults and interpolated, `dir` vectors read from under root.
// structural problems (unknown keys, bad shapes, unknown template variables)
// are errors here, each with its line; semantic ones are Validate's.
func ParseIn(root string, data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c := &Config{}
	if len(doc.Content) == 0 {
		return c, nil // empty file
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: top level must be a mapping of vectors", top.Line)
	}
	var errs errList
	for i := 0; i < len(top.Content); i += 2 {
		id, node := top.Content[i].Value, top.Content[i+1]
		if err := c.loadVector(root, id, node); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	return c, errs.err()
}

// vectorKeys are the only keys a vector takes.
var vectorKeys = map[string]bool{"kind": true, "defaults": true, "items": true, "dir": true}

// taskFile marks a directory under a task vector's `dir` as one task.
const taskFile = "task.yaml"

// loadVector decodes one top-level entry and appends its items to c.
func (c *Config) loadVector(root, id string, node *yaml.Node) error {
	var errs errList
	fields, err := mappingOf(node, "vector "+id)
	if err != nil {
		return err
	}
	for k, v := range fields {
		if !vectorKeys[k] {
			errs.add("line %d: vector %q: unknown key %q (kind, defaults, items, dir)", v.Line, id, k)
		}
	}
	kindNode, ok := fields["kind"]
	if !ok {
		return fmt.Errorf("line %d: vector %q: missing kind (%s)", node.Line, id, strings.Join(Kinds, "|"))
	}
	kind := kindNode.Value
	proto := prototype(kind)
	if proto == nil {
		return fmt.Errorf("line %d: vector %q: unknown kind %q (%s)", kindNode.Line, id, kind, strings.Join(Kinds, "|"))
	}

	if dirNode, ok := fields["dir"]; ok {
		if kind != "task" {
			errs.add("line %d: vector %q: dir is only supported on task vectors", dirNode.Line, id)
			return errs.err()
		}
		for _, k := range []string{"items", "defaults"} {
			if v, ok := fields[k]; ok {
				errs.add("line %d: vector %q: %s and dir are mutually exclusive -- a task in a directory is configured by its own %s", v.Line, id, k, taskFile)
			}
		}
		if err := c.loadDir(root, id, dirNode); err != nil {
			errs.errs = append(errs.errs, err)
		}
		return errs.err()
	}

	known := yamlKeys(reflect.TypeOf(proto).Elem())
	defaults := map[string]*yaml.Node{}
	if d, ok := fields["defaults"]; ok && !isNull(d) {
		what := fmt.Sprintf("vector %q defaults", id)
		defaults, err = fieldsOf(d, known, what)
		if err == nil {
			err = checkSlots(defaults, kind, what)
		}
		if err != nil {
			errs.errs = append(errs.errs, err)
		}
	}

	itemsNode, ok := fields["items"]
	if !ok || isNull(itemsNode) {
		errs.add("line %d: vector %q: items or dir is required", node.Line, id)
		return errs.err()
	}
	items, err := mappingOf(itemsNode, fmt.Sprintf("vector %q items", id))
	if err != nil {
		errs.errs = append(errs.errs, err)
		return errs.err()
	}
	if len(items) == 0 {
		errs.add("line %d: vector %q: items is empty", itemsNode.Line, id)
	}
	for _, itemID := range orderedKeys(itemsNode) {
		if err := c.loadItem(kind, id, itemID, "", items[itemID], defaults); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	return errs.err()
}

// loadItem decodes one item node, merges it over defaults, interpolates it and
// appends it to c. dir is where it was defined, relative to the pats root.
func (c *Config) loadItem(kind, vector, id, dir string, node *yaml.Node, defaults map[string]*yaml.Node) error {
	proto := prototype(kind)
	known := yamlKeys(reflect.TypeOf(proto).Elem())
	what := fmt.Sprintf("%s %q", kind, id)
	own := map[string]*yaml.Node{}
	if !isNull(node) {
		var err error
		if own, err = fieldsOf(node, known, what); err != nil {
			return err
		}
		if err := checkSlots(own, kind, what); err != nil {
			return err
		}
	}
	item, err := merge(proto, kind, defaults, own)
	if err != nil {
		return fmt.Errorf("line %d: %s: %w", node.Line, what, err)
	}
	if err := interpolate(item, kind, id, vector); err != nil {
		return fmt.Errorf("line %d: %s: %w", node.Line, what, err)
	}
	c.append(kind, id, vector, dir, item)
	return nil
}

// loadDir reads a task vector's `dir`: every directory under it holding a
// task.yaml is one task, its id the directory's name, in lexical path order.
// directories starting with "." are skipped; a task.yaml below another
// task's directory is an error.
func (c *Config) loadDir(root, vector string, dirNode *yaml.Node) error {
	rel := filepath.Clean(dirNode.Value)
	if dirNode.Value == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("line %d: vector %q: dir %q must be a path inside the pats root", dirNode.Line, vector, dirNode.Value)
	}
	base := filepath.Join(root, rel)
	if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
		return fmt.Errorf("line %d: vector %q: dir %q is not a directory", dirNode.Line, vector, dirNode.Value)
	}
	var errs errList
	var found []string // task dirs, relative to root
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != base && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, taskFile)); err != nil {
			return nil
		}
		taskDir, _ := filepath.Rel(root, path)
		for _, f := range found {
			if strings.HasPrefix(taskDir, f+string(filepath.Separator)) {
				errs.add("%s: nested in task %s -- a task directory may not hold another task", filepath.Join(taskDir, taskFile), filepath.Join(f, taskFile))
				return nil
			}
		}
		found = append(found, taskDir)
		return nil
	})
	if err != nil {
		errs.add("vector %q: read dir %q: %v", vector, dirNode.Value, err)
	}
	if len(found) == 0 && len(errs.errs) == 0 {
		errs.add("line %d: vector %q: dir %q holds no %s", dirNode.Line, vector, dirNode.Value, taskFile)
	}
	for _, taskDir := range found {
		if err := c.loadTaskFile(root, vector, taskDir); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	return errs.err()
}

// loadTaskFile parses one task.yaml into a task item. errors are prefixed with
// the file's path relative to the pats root.
func (c *Config) loadTaskFile(root, vector, taskDir string) error {
	file := filepath.Join(taskDir, taskFile)
	data, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Line: 1} // an empty file is an empty task
	if len(doc.Content) > 0 {
		node = doc.Content[0]
	}
	if err := c.loadItem("task", vector, filepath.Base(taskDir), taskDir, node, nil); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// prototype returns a pointer to a zero item struct of the kind, or nil.
func prototype(kind string) any {
	switch kind {
	case "sandbox":
		return &Sandbox{}
	case "agent":
		return &Agent{}
	case "task":
		return &Task{}
	case "scorer":
		return &Scorer{}
	case "suite":
		return &Suite{}
	}
	return nil
}

// append stores a merged item under its kind, stamping id, vector and dir.
func (c *Config) append(kind, id, vector, dir string, item any) {
	switch v := item.(type) {
	case *Sandbox:
		v.ID, v.Vector = id, vector
		if v.Driver == "" {
			v.Driver = "docker"
		}
		c.Sandboxes = append(c.Sandboxes, *v)
	case *Agent:
		v.ID, v.Vector = id, vector
		c.Agents = append(c.Agents, *v)
	case *Task:
		v.ID, v.Vector, v.Dir = id, vector, dir
		c.Tasks = append(c.Tasks, *v)
	case *Scorer:
		v.ID, v.Vector, v.Dir = id, vector, dir
		c.Scorers = append(c.Scorers, *v)
	case *Suite:
		v.ID, v.Vector = id, vector
		c.Suites = append(c.Suites, *v)
	}
}

// slots groups the keys that are alternative forms of one field: an item sets
// at most one key per slot, and a slot merges over defaults as a unit.
var slots = map[string][][]string{
	"task": {
		{"prompt", "prompt-file", "prompt-cmd"},
		{"stage", "stage-cmd"},
		{"prepare", "prepare-cmd"},
		{"collect", "collect-cmd"},
	},
	"scorer": {
		{"score", "score-cmd"},
		{"prompt", "prompt-file", "prompt-cmd"},
	},
}

// checkSlots rejects one layer setting two keys of the same slot.
func checkSlots(fields map[string]*yaml.Node, kind, what string) error {
	var errs errList
	for _, slot := range slots[kind] {
		var set []string
		line := 0
		for _, k := range slot {
			if v, ok := fields[k]; ok && !isNull(v) {
				set = append(set, k)
				line = v.Line
			}
		}
		if len(set) > 1 {
			errs.add("line %d: %s: %s are mutually exclusive", line, what, strings.Join(set, ", "))
		}
	}
	return errs.err()
}

// merge decodes defaults overlaid with own into a fresh item of proto's type.
// an own null unsets the default. a slot set or nulled by own replaces the
// whole slot; env merges key by key. the overlay is built as a mapping node so
// aliases and merge keys in either layer decode as yaml.v3 resolves them.
func merge(proto any, kind string, defaults, own map[string]*yaml.Node) (any, error) {
	layer := map[string]*yaml.Node{}
	for k, v := range defaults {
		if !isNull(v) {
			layer[k] = v
		}
	}
	for _, slot := range slots[kind] {
		touched := false
		for _, k := range slot {
			if _, ok := own[k]; ok {
				touched = true
			}
		}
		if touched {
			for _, k := range slot {
				delete(layer, k)
			}
		}
	}
	for k, v := range own {
		switch {
		case isNull(v):
			delete(layer, k)
		case k == "env" && layer["env"] != nil:
			env, err := mergeEnv(layer["env"], v)
			if err != nil {
				return nil, err
			}
			layer[k] = env
		default:
			layer[k] = v
		}
	}
	keys := make([]string, 0, len(layer))
	for k := range layer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, layer[k])
	}
	item := reflect.New(reflect.TypeOf(proto).Elem()).Interface()
	if err := m.Decode(item); err != nil {
		return nil, err
	}
	return item, nil
}

// mergeEnv overlays one env mapping on another, key by key; a null value
// drops the inherited key.
func mergeEnv(base, over *yaml.Node) (*yaml.Node, error) {
	b, err := mappingOf(base, "env")
	if err != nil {
		return nil, err
	}
	o, err := mappingOf(over, "env")
	if err != nil {
		return nil, err
	}
	for k, v := range o {
		if isNull(v) {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, b[k])
	}
	return m, nil
}

// mappingOf decodes node as a mapping of field -> value node, resolving
// aliases and merge keys. what names the node in errors.
func mappingOf(node *yaml.Node, what string) (map[string]*yaml.Node, error) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: %s: must be a mapping", node.Line, what)
	}
	// yaml.v3 fills a yaml.Node value, not a *yaml.Node, so decode values and
	// hand out pointers to the copies.
	var m map[string]yaml.Node
	if err := node.Decode(&m); err != nil {
		return nil, fmt.Errorf("line %d: %s: %w", node.Line, what, err)
	}
	out := make(map[string]*yaml.Node, len(m))
	for k, v := range m {
		v := v
		out[k] = &v
	}
	return out, nil
}

// fieldsOf is mappingOf plus a check that every key is a field of the kind.
func fieldsOf(node *yaml.Node, known map[string]bool, what string) (map[string]*yaml.Node, error) {
	m, err := mappingOf(node, what)
	if err != nil {
		return nil, err
	}
	var errs errList
	for k, v := range m {
		if !known[k] {
			errs.add("line %d: %s: unknown field %q", v.Line, what, k)
		}
	}
	return m, errs.err()
}

// orderedKeys lists a mapping node's keys in file order (merge keys and
// aliases excluded -- items are never shared that way).
func orderedKeys(node *yaml.Node) []string {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	return keys
}

func isNull(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// yamlKeys collects the yaml field names of a struct type ("-" skipped; an
// inline struct contributes its own fields).
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if opts == "inline" && f.Type.Kind() == reflect.Struct {
			for k := range yamlKeys(f.Type) {
				keys[k] = true
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if name != "-" {
			keys[name] = true
		}
	}
	return keys
}

var tmplVar = regexp.MustCompile(`\$\{([^}]*)\}`)

// interpolate expands ${<kind>} (the item's id) and ${vector} in the string
// fields of item, nested structs included. fields tagged `tmpl:"-"` are
// content (inline prompts, script bodies, env) and are left alone; suites
// hold references, not templates. an unknown variable is an error.
func interpolate(item any, kind, id, vector string) error {
	if kind == "suite" {
		return nil
	}
	vars := map[string]string{kind: id, "vector": vector}
	var errs errList
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			out := tmplVar.ReplaceAllStringFunc(v.String(), func(m string) string {
				name := m[2 : len(m)-1]
				if val, ok := vars[name]; ok {
					return val
				}
				errs.add("unknown variable ${%s} (${%s}, ${vector})", name, kind)
				return m
			})
			v.SetString(out)
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if t.Field(i).Tag.Get("tmpl") == "-" {
					continue
				}
				walk(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(item).Elem())
	return errs.err()
}

func (s *StrList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var single string
		if err := node.Decode(&single); err != nil {
			return err
		}
		*s = StrList{single}
	case yaml.SequenceNode:
		var many []string
		if err := node.Decode(&many); err != nil {
			return err
		}
		*s = StrList(many)
	default:
		return fmt.Errorf("expected a scalar or list, got yaml kind %d", node.Kind)
	}
	return nil
}
