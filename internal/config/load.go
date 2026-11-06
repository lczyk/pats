package config

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a pats.yaml body into a Config: every vector's items merged
// with its defaults and interpolated. structural problems (unknown keys, bad
// shapes, unknown template variables) are errors here, each with its line;
// semantic ones are Validate's.
func Parse(data []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c := &Config{}
	if len(root.Content) == 0 {
		return c, nil // empty file
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: top level must be a mapping of vectors", top.Line)
	}
	var errs errList
	for i := 0; i < len(top.Content); i += 2 {
		id, node := top.Content[i].Value, top.Content[i+1]
		if err := c.loadVector(id, node); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	return c, errs.err()
}

// vectorKeys are the only keys a vector takes.
var vectorKeys = map[string]bool{"kind": true, "defaults": true, "items": true}

// loadVector decodes one top-level entry and appends its items to c.
func (c *Config) loadVector(id string, node *yaml.Node) error {
	var errs errList
	fields, err := mappingOf(node, "vector "+id)
	if err != nil {
		return err
	}
	for k, v := range fields {
		if !vectorKeys[k] {
			errs.add("line %d: vector %q: unknown key %q (kind, defaults, items)", v.Line, id, k)
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
	known := yamlKeys(reflect.TypeOf(proto).Elem())

	defaults := map[string]*yaml.Node{}
	if d, ok := fields["defaults"]; ok && !isNull(d) {
		defaults, err = fieldsOf(d, known, fmt.Sprintf("vector %q defaults", id))
		if err != nil {
			errs.errs = append(errs.errs, err)
		}
	}

	itemsNode, ok := fields["items"]
	if !ok || isNull(itemsNode) {
		errs.add("line %d: vector %q: items is required", node.Line, id)
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
		itemNode := items[itemID]
		own := map[string]*yaml.Node{}
		if !isNull(itemNode) {
			own, err = fieldsOf(itemNode, known, fmt.Sprintf("%s %q", kind, itemID))
			if err != nil {
				errs.errs = append(errs.errs, err)
				continue
			}
		}
		item, err := merge(proto, defaults, own)
		if err != nil {
			errs.add("line %d: %s %q: %v", itemNode.Line, kind, itemID, err)
			continue
		}
		if err := interpolate(item, kind, itemID, id); err != nil {
			errs.add("line %d: %s %q: %v", itemNode.Line, kind, itemID, err)
			continue
		}
		c.append(kind, itemID, id, item)
	}
	return errs.err()
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

// append stores a merged item under its kind, stamping id + vector.
func (c *Config) append(kind, id, vector string, item any) {
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
		v.ID, v.Vector = id, vector
		c.Tasks = append(c.Tasks, *v)
	case *Scorer:
		v.ID, v.Vector = id, vector
		c.Scorers = append(c.Scorers, *v)
	case *Suite:
		v.ID, v.Vector = id, vector
		c.Suites = append(c.Suites, *v)
	}
}

// merge decodes defaults overlaid with own into a fresh item of proto's type.
// an own null unsets the default. the overlay is built as a mapping node so
// aliases and merge keys in either layer decode as yaml.v3 resolves them.
func merge(proto any, defaults, own map[string]*yaml.Node) (any, error) {
	layer := map[string]*yaml.Node{}
	for k, v := range defaults {
		if !isNull(v) {
			layer[k] = v
		}
	}
	for k, v := range own {
		if isNull(v) {
			delete(layer, k)
		} else {
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

// yamlKeys collects the yaml field names of a struct type ("-" skipped).
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = strings.ToLower(t.Field(i).Name)
		}
		if name != "-" {
			keys[name] = true
		}
	}
	return keys
}

var tmplVar = regexp.MustCompile(`\$\{([^}]*)\}`)

// interpolate expands ${<kind>} (the item's id) and ${vector} in every string
// field of item, nested structs included. suites hold references, not
// templates, and are left alone. an unknown variable is an error.
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
			for i := 0; i < v.NumField(); i++ {
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
