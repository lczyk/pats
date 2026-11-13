package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lczyk/pats/internal/machine"
)

// HarnessItem names one harness a pats.yaml defines. its definition is laid
// over the config's harness tables (docs/spec/harness-registry.md).
type HarnessItem struct {
	Name   string
	Vector string
}

// harnessLayer is a harness item as written: its vector's defaults under its
// own keys, a null kept for a key it unsets.
type harnessLayer struct {
	item machine.Item
	line int
}

// loadHarnessVector reads a vector of kind harness. an item is not decoded
// here: it is kept as written and laid over the machine's harnesses once the
// whole file is read, so a key it leaves out, one it nulls and one it sets
// false stay three things.
func (c *Config) loadHarnessVector(name string, fields map[string]*yaml.Node, node *yaml.Node) error {
	var errs errList
	if d, ok := fields["dir"]; ok {
		errs.add("line %d: vector %q: dir is only supported on task vectors", d.Line, name)
		return errs.err()
	}
	known := machine.HarnessKeys()
	defaults := map[string]*yaml.Node{}
	if d, ok := fields["defaults"]; ok && !isNull(d) {
		var err error
		if defaults, err = fieldsOf(d, known, fmt.Sprintf("vector %q defaults", name)); err != nil {
			errs.errs = append(errs.errs, err)
		}
	}
	itemsNode, ok := fields["items"]
	if !ok || isNull(itemsNode) {
		errs.add("line %d: vector %q: items is required", node.Line, name)
		return errs.err()
	}
	if itemsNode.Kind == yaml.AliasNode {
		itemsNode = itemsNode.Alias
	}
	if itemsNode.Kind != yaml.MappingNode {
		errs.add("line %d: vector %q items: must be a mapping", itemsNode.Line, name)
		return errs.err()
	}
	if len(itemsNode.Content) == 0 {
		errs.add("line %d: vector %q: items is empty", itemsNode.Line, name)
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
		own := map[string]*yaml.Node{}
		if !isNull(item) {
			var err error
			if own, err = fieldsOf(item, known, fmt.Sprintf("harness %q", k.Value)); err != nil {
				errs.errs = append(errs.errs, err)
				continue
			}
		}
		layer := map[string]*yaml.Node{}
		for key, v := range defaults {
			layer[key] = v
		}
		for key, v := range own {
			layer[key] = v
		}
		keys := make([]string, 0, len(layer))
		for key := range layer {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		m := &yaml.Node{Kind: yaml.MappingNode, Line: k.Line}
		for _, key := range keys {
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, Line: layer[key].Line}, layer[key])
		}
		c.HarnessItems = append(c.HarnessItems, HarnessItem{Name: k.Value, Vector: name})
		c.harnessLayers = append(c.harnessLayers, harnessLayer{item: machine.Item{Name: k.Value, Node: m}, line: k.Line})
	}
	return errs.err()
}

// projectHarnesses lays the file's harness items over the config's tables.
// an item named twice is left to Validate, which names both.
func (c *Config) projectHarnesses() error {
	if len(c.harnessLayers) == 0 {
		return nil
	}
	lines := map[string]int{}
	var items []machine.Item
	for _, l := range c.harnessLayers {
		if _, dup := lines[l.item.Name]; dup {
			continue
		}
		lines[l.item.Name] = l.line
		items = append(items, l.item)
	}
	err := c.Harnesses.Project(items)
	if err == nil {
		return nil
	}
	// each of the tables' errors starts with its entry: put the item's line
	// before the ones that are this file's.
	names := make([]string, 0, len(lines))
	for n := range lines {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	var errs errList
	for _, msg := range strings.Split(err.Error(), "\n") {
		line := 0
		for _, n := range names {
			if id := "harnesses." + n; strings.HasPrefix(msg, id+":") || strings.HasPrefix(msg, id+".") {
				line = lines[n]
				break
			}
		}
		if line > 0 {
			errs.add("line %d: %s", line, msg)
		} else {
			errs.add("%s", msg)
		}
	}
	return errs.err()
}
