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

	"github.com/lczyk/pats/internal/machine"
)

// Parse decodes a pats.yaml body whose `dir` vectors resolve against the
// current directory. see ParseIn.
func Parse(data []byte) (*Config, error) { return ParseIn(".", data) }

// ParseIn decodes a pats.yaml body into a Config: every vector's items merged
// with its defaults and checked for templating, `dir` vectors read from under root.
// structural problems (unknown keys, bad shapes, unknown template variables)
// are errors here, each with its line; semantic ones are Validate's. its
// harnesses are the defaults: Load is what reads the machine's file.
func ParseIn(root string, data []byte) (*Config, error) {
	return parseIn(root, data, machine.Defaults())
}

// parseIn is ParseIn over the given harness tables.
func parseIn(root string, data []byte, tables *machine.Tables) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c := &Config{Harnesses: tables}
	if len(doc.Content) == 0 {
		return c, nil // empty file
	}
	if err := checkKeys(&doc); err != nil {
		return nil, err
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: top level must be a mapping of vectors", top.Line)
	}
	var errs errList
	for i := 0; i < len(top.Content); i += 2 {
		name, node := top.Content[i].Value, top.Content[i+1]
		if err := c.loadVector(root, name, node); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	if err := c.projectHarnesses(); err != nil {
		errs.errs = append(errs.errs, err)
	}
	return c, errs.err()
}

// vectorKeys are the only keys a vector takes.
var vectorKeys = map[string]bool{"kind": true, "defaults": true, "items": true, "dir": true}

// taskFile marks a directory under a task vector's `dir` as one task.
const taskFile = "task.yaml"

// loadVector decodes one top-level entry and appends its items to c.
func (c *Config) loadVector(root, name string, node *yaml.Node) error {
	var errs errList
	fields, err := mappingOf(node, "vector "+name)
	if err != nil {
		return err
	}
	for k, v := range fields {
		if !vectorKeys[k] {
			errs.add("line %d: vector %q: unknown key %q (kind, defaults, items, dir)", v.Line, name, k)
		}
	}
	kindNode, ok := fields["kind"]
	if !ok {
		return fmt.Errorf("line %d: vector %q: missing kind (%s)", node.Line, name, strings.Join(Kinds, "|"))
	}
	kind := kindNode.Value
	if kind == "harness" {
		if err := c.loadHarnessVector(name, fields, node); err != nil {
			errs.errs = append(errs.errs, err)
		}
		return errs.err()
	}
	proto := prototype(kind)
	if proto == nil {
		return fmt.Errorf("line %d: vector %q: unknown kind %q (%s)", kindNode.Line, name, kind, strings.Join(Kinds, "|"))
	}

	if dirNode, ok := fields["dir"]; ok {
		if kind != "task" {
			errs.add("line %d: vector %q: dir is only supported on task vectors", dirNode.Line, name)
			return errs.err()
		}
		for _, k := range []string{"items", "defaults"} {
			if v, ok := fields[k]; ok {
				errs.add("line %d: vector %q: %s and dir are mutually exclusive -- a task in a directory is configured by its own %s", v.Line, name, k, taskFile)
			}
		}
		if m := templateVar.FindStringSubmatch(dirNode.Value); m != nil {
			errs.add("line %d: vector %q: dir: ${%s}: pats.yaml is not templated", dirNode.Line, name, m[1])
			return errs.err()
		}
		if err := c.loadDir(root, name, dirNode); err != nil {
			errs.errs = append(errs.errs, err)
		}
		return errs.err()
	}

	known := yamlKeys(reflect.TypeOf(proto).Elem())
	defaults := map[string]*yaml.Node{}
	if d, ok := fields["defaults"]; ok && !isNull(d) {
		what := fmt.Sprintf("vector %q defaults", name)
		defaults, err = fieldsOf(d, known, what)
		if err == nil {
			err = checkSlots(defaults, kind, what)
		}
		// on their own: an item that overrides a field must not hide a
		// placeholder its default holds.
		if err == nil {
			var only any
			if only, err = merge(prototype(kind), kind, defaults, nil); err == nil {
				err = checkTemplates(only, kind)
			}
		}
		if err != nil {
			errs.errs = append(errs.errs, fmt.Errorf("line %d: %s: %w", d.Line, what, err))
		}
	}

	itemsNode, ok := fields["items"]
	if !ok || isNull(itemsNode) {
		errs.add("line %d: vector %q: items or dir is required", node.Line, name)
		return errs.err()
	}
	items, err := mappingOf(itemsNode, fmt.Sprintf("vector %q items", name))
	if err != nil {
		errs.errs = append(errs.errs, err)
		return errs.err()
	}
	if len(items) == 0 {
		errs.add("line %d: vector %q: items is empty", itemsNode.Line, name)
	}
	// names and nodes both from the file, in its order: a key yaml decodes
	// to something other than its text (a null, a binary, a merge) would
	// name one item and hand it another's node, or none.
	if itemsNode.Kind == yaml.AliasNode {
		itemsNode = itemsNode.Alias
	}
	for i := 0; i+1 < len(itemsNode.Content); i += 2 {
		k, item := itemsNode.Content[i], itemsNode.Content[i+1]
		if k.Kind == yaml.AliasNode {
			k = k.Alias
		}
		if tag := k.ShortTag(); k.Kind != yaml.ScalarNode || tag == "!!null" || tag == "!!binary" || tag == "!!merge" {
			errs.add("line %d: vector %q items: an item is keyed by its name, not by a %s key", k.Line, name, strings.TrimPrefix(tag, "!!"))
			continue
		}
		if err := c.loadItem(kind, name, k.Value, "", item, defaults); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	return errs.err()
}

// loadItem decodes one item node, merges it over defaults, checks it holds no
// placeholder and appends it to c. dir is where it was defined, relative to the pats root.
func (c *Config) loadItem(kind, vector, name, dir string, node *yaml.Node, defaults map[string]*yaml.Node) error {
	proto := prototype(kind)
	known := yamlKeys(reflect.TypeOf(proto).Elem())
	what := fmt.Sprintf("%s %q", kind, name)
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
	if err := checkTemplates(item, kind); err != nil {
		return fmt.Errorf("line %d: %s: %w", node.Line, what, err)
	}
	c.append(kind, name, vector, dir, item)
	return nil
}

// loadDir reads a task vector's `dir`: every directory under it holding a
// task.yaml is one task, its name the directory's name, in lexical path order.
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
	if err := checkKeys(&doc); err != nil {
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

// append stores a merged item under its kind, stamping name, vector and dir.
func (c *Config) append(kind, name, vector, dir string, item any) {
	switch v := item.(type) {
	case *Sandbox:
		v.Name, v.Vector = name, vector
		if v.Driver == "" {
			v.Driver = "docker"
		}
		c.Sandboxes = append(c.Sandboxes, *v)
	case *Agent:
		v.Name, v.Vector = name, vector
		c.Agents = append(c.Agents, *v)
	case *Task:
		v.Name, v.Vector, v.Dir = name, vector, dir
		c.Tasks = append(c.Tasks, *v)
	case *Scorer:
		v.Name, v.Vector, v.Dir = name, vector, dir
		c.Scorers = append(c.Scorers, *v)
	case *Suite:
		v.Name, v.Vector = name, vector
		c.Suites = append(c.Suites, *v)
	}
}

// slots groups the keys that are alternative forms of one field: an item sets
// at most one key per slot, and a slot merges over defaults as a unit.
var slots = map[string][][]string{
	"task":   {{"prompt", "prompt-file", "prompt-cmd"}},
	"scorer": {{"prompt", "prompt-file", "prompt-cmd"}},
}

// removed are the keys a step's -cmd form had, and what replaced them: every
// step is one field, a shell script (docs/spec/steps.md).
var removed = map[string]string{
	"stage-cmd":     "stage",
	"prepare-cmd":   "prepare",
	"collect-cmd":   "collect",
	"score-cmd":     "score",
	"provision-cmd": "provision",
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
		switch {
		case known[k]:
		case removed[k] != "":
			errs.add("line %d: %s: %s is gone: use %s, a shell script (`%s: ./script.sh --flag` runs that file)", v.Line, what, k, removed[k], removed[k])
		default:
			errs.add("line %d: %s: unknown field %q", v.Line, what, k)
		}
	}
	return m, errs.err()
}

// checkKeys refuses a mapping keyed by a mapping or a list, anywhere in the
// document. nothing in a pats file is keyed that way, and yaml.v3 panics
// decoding one that sits beside a merge key.
func checkKeys(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a key must be a plain value, not a mapping or a list", n.Content[i].Line)
			}
		}
	}
	for _, c := range n.Content {
		if err := checkKeys(c); err != nil {
			return err
		}
	}
	return nil
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

// templateEnv is the step env var carrying what a ${...} pats once expanded
// in pats.yaml named, on an item of kind; "" when no step gets it.
func templateEnv(name, kind string) string {
	if kind != "task" && kind != "scorer" {
		return "" // an agent runs no step of its own, a provision none of these
	}
	switch name {
	case "task":
		return "PATS_TASK_NAME"
	case "agent":
		return "PATS_AGENT_NAME"
	case "scorer":
		return "PATS_SCORER_NAME"
	case "vector":
		switch kind {
		case "task":
			return "PATS_TASK_VECTOR"
		case "scorer":
			return "PATS_SCORER_VECTOR"
		}
	}
	return ""
}

var templateVar = regexp.MustCompile(`\$\{(task|agent|scorer|sandbox|vector)\}`)

// checkTemplates refuses a ${task}-style placeholder in any string field of
// item, nested structs, lists and maps included: pats expands none, and a
// config written for when it did must fail rather than read a file named
// prompts/${task}.txt. fields tagged `text:"free"` (a prompt's text, the
// agent's to read) are left alone; suites hold references, not text.
func checkTemplates(item any, kind string) error {
	if kind == "suite" {
		return nil
	}
	var errs errList
	seen := map[string]bool{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			for _, m := range templateVar.FindAllStringSubmatch(v.String(), -1) {
				if seen[m[1]] {
					continue
				}
				seen[m[1]] = true
				if env := templateEnv(m[1], kind); env != "" {
					errs.add("${%s}: pats.yaml is not templated; a step reads the name from $%s", m[1], env)
				} else {
					errs.add("${%s}: pats.yaml is not templated, and no step env var carries it", m[1])
				}
			}
		case reflect.Pointer:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if t.Field(i).Tag.Get("text") == "free" || !t.Field(i).IsExported() {
					continue
				}
				walk(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k))
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
