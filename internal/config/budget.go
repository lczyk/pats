package config

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Budget caps what a pair, or an invocation, may use, by kind
// (docs/spec/budget-kinds.md): on an agent it caps each of its pairs, on
// --budget the invocation. a zero kind is no cap of that kind.
type Budget struct {
	Cost   float64       // us dollars
	Tokens int64         // reported tokens: input, output, cache reads and writes
	Tools  int64         // reported tool calls
	Turns  int64         // reported turns
	Time   time.Duration // wall-clock
}

// BudgetKinds are the kinds in the order they are checked: the first one a
// check finds passed is the one that stopped it.
var BudgetKinds = []string{"cost", "tokens", "tools", "turns", "time"}

// IsZero says whether the budget caps nothing.
func (b Budget) IsZero() bool { return b == Budget{} }

// String lists the caps given, as --budget takes them.
func (b Budget) String() string {
	var parts []string
	if b.Cost > 0 {
		parts = append(parts, "cost="+strconv.FormatFloat(b.Cost, 'f', -1, 64))
	}
	if b.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens=%d", b.Tokens))
	}
	if b.Tools > 0 {
		parts = append(parts, fmt.Sprintf("tools=%d", b.Tools))
	}
	if b.Turns > 0 {
		parts = append(parts, fmt.Sprintf("turns=%d", b.Turns))
	}
	if b.Time > 0 {
		parts = append(parts, "time="+b.Time.String())
	}
	return strings.Join(parts, ",")
}

// maxCount bounds a tokens, tools or turns cap.
const maxCount = 1e12

// CheckCost says whether a cap in us dollars is one: finite, above zero, at
// most a million.
func CheckCost(c float64) error {
	if math.IsNaN(c) || math.IsInf(c, 0) || c <= 0 || c > 1e6 {
		return fmt.Errorf("%v is not a cost cap (a number above 0, at most 1e6)", c)
	}
	return nil
}

var countRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kKmMgG]?)$`)

// ParseCount reads a tokens, tools or turns cap: a whole number above zero,
// at most 1e12, that may carry a decimal suffix (k, m or g, either case)
// after a decimal number, as long as the product is whole.
func ParseCount(s string) (int64, error) {
	bad := fmt.Errorf("%q is not a count (a whole number above 0, at most 1e12, e.g. 300, 20k, 1.5M)", s)
	m := countRe.FindStringSubmatch(s)
	if m == nil {
		return 0, bad
	}
	r, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, bad
	}
	exp := map[string]int64{"": 0, "k": 3, "m": 6, "g": 9}[strings.ToLower(m[2])]
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(exp), nil)))
	if !r.IsInt() || r.Sign() <= 0 || r.Cmp(new(big.Rat).SetFloat64(maxCount)) > 0 {
		return 0, bad
	}
	return r.Num().Int64(), nil
}

// parseTime reads a time cap: a go duration above zero.
func parseTime(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a duration above 0 (e.g. 90s, 5m, 2h30m)", s)
	}
	return d, nil
}

var plainCostRe = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// parseFlagCost reads a cost on the command line: a plain decimal number.
func parseFlagCost(s string) (float64, error) {
	if !plainCostRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a cost cap (a number above 0, at most 1e6)", s)
	}
	c, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a cost cap (a number above 0, at most 1e6)", s)
	}
	return c, CheckCost(c)
}

// ParseBudget reads --budget: a bare number, which is the cost, or a
// comma-separated list of kind=value.
func ParseBudget(s string) (Budget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Budget{}, fmt.Errorf("empty: want a cost or kind=value[,kind=value] (kinds: %s)", strings.Join(BudgetKinds, ", "))
	}
	if !strings.Contains(s, "=") {
		c, err := parseFlagCost(s)
		if err != nil {
			return Budget{}, fmt.Errorf("cost: %w", err)
		}
		return Budget{Cost: c}, nil
	}
	var b Budget
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return Budget{}, fmt.Errorf("%q is not kind=value (kinds: %s)", part, strings.Join(BudgetKinds, ", "))
		}
		if seen[k] {
			return Budget{}, fmt.Errorf("%s: given twice", k)
		}
		seen[k] = true
		if v == "" {
			return Budget{}, fmt.Errorf("%s: no value", k)
		}
		var err error
		switch k {
		case "cost":
			b.Cost, err = parseFlagCost(v)
		case "tokens":
			b.Tokens, err = ParseCount(v)
		case "tools":
			b.Tools, err = ParseCount(v)
		case "turns":
			b.Turns, err = ParseCount(v)
		case "time":
			b.Time, err = parseTime(v)
		default:
			return Budget{}, fmt.Errorf("unknown kind %q (kinds: %s)", k, strings.Join(BudgetKinds, ", "))
		}
		if err != nil {
			return Budget{}, fmt.Errorf("%s: %w", k, err)
		}
	}
	return b, nil
}

// UnmarshalYAML takes a number, the cost, or a mapping of kinds. a kind
// that is null is no cap of it, as is an empty mapping.
func (b *Budget) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.ScalarNode:
		c, err := yamlCost(node)
		if err != nil {
			return fmt.Errorf("line %d: budget: %w", node.Line, err)
		}
		*b = Budget{Cost: c}
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("line %d: budget: want a cost or a mapping of %s", node.Line, strings.Join(BudgetKinds, ", "))
	}
	m, err := mappingOf(node, "budget")
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if err := checkBudgetKinds(m); err != nil {
		return err
	}
	var out Budget
	var errs errList
	for _, k := range keys {
		v := resolved(m[k])
		if isNull(v) {
			continue
		}
		var err error
		switch k {
		case "cost":
			out.Cost, err = yamlCost(v)
		case "tokens":
			out.Tokens, err = yamlCount(v)
		case "tools":
			out.Tools, err = yamlCount(v)
		case "turns":
			out.Turns, err = yamlCount(v)
		case "time":
			out.Time, err = yamlTime(v)
		}
		if err != nil {
			errs.add("line %d: budget: %s: %v", v.Line, k, err)
		}
	}
	if err := errs.err(); err != nil {
		return err
	}
	*b = out
	return nil
}

// checkBudgetKinds rejects a key that is no kind, a null one included:
// a misspelt kind unset is still misspelt.
func checkBudgetKinds(m map[string]*yaml.Node) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs errList
	for _, k := range keys {
		known := false
		for _, kind := range BudgetKinds {
			known = known || k == kind
		}
		if !known {
			errs.add("line %d: budget: unknown kind %q (kinds: %s)", m[k].Line, k, strings.Join(BudgetKinds, ", "))
		}
	}
	return errs.err()
}

// resolved is the node an alias stands for, or the node itself.
func resolved(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.AliasNode {
		return n.Alias
	}
	return n
}

func yamlCost(n *yaml.Node) (float64, error) {
	var c float64
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float") || n.Decode(&c) != nil {
		return 0, fmt.Errorf("%q is not a cost cap (a number above 0, at most 1e6)", n.Value)
	}
	return c, CheckCost(c)
}

func yamlCount(n *yaml.Node) (int64, error) {
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!str") {
		return 0, fmt.Errorf("%q is not a count (a whole number above 0, at most 1e12, e.g. 300, 20k, 1.5M)", n.Value)
	}
	if n.Tag == "!!int" {
		var c int64
		if err := n.Decode(&c); err != nil || c <= 0 || c > maxCount {
			return 0, fmt.Errorf("%q is not a count (a whole number above 0, at most 1e12, e.g. 300, 20k, 1.5M)", n.Value)
		}
		return c, nil
	}
	return ParseCount(n.Value)
}

func yamlTime(n *yaml.Node) (time.Duration, error) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return 0, fmt.Errorf("%q is not a duration above 0 (e.g. 90s, 5m, 2h30m)", n.Value)
	}
	return parseTime(n.Value)
}

// budgetMapping is a budget node as a mapping: a number is {cost: number},
// so a default's other kinds survive an item's bare cost.
func budgetMapping(n *yaml.Node) *yaml.Node {
	n = resolved(n)
	if n.Kind != yaml.ScalarNode {
		return n
	}
	return &yaml.Node{Kind: yaml.MappingNode, Line: n.Line, Column: n.Column, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cost", Line: n.Line, Column: n.Column}, n,
	}}
}
