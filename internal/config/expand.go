package config

import (
	"errors"
	"fmt"
)

// TestPair is one expanded (agent, task) pair of a suite.
type TestPair struct {
	Agent string
	Task  string
}

// ScorePair is one expanded (task, scorer) combination of a suite.
type ScorePair struct {
	Task   string
	Scorer string
}

// sets maps every name of a kind to the item names it stands for: an item name to
// itself, a vector name to its items in file order. a vector sharing its name
// with its sole item maps once (same set either way); validateNames rejects
// every other overlap.
func (c *Config) sets(kind string) map[string][]string {
	out := map[string][]string{}
	add := func(name, vector string) {
		out[name] = append(out[name], name)
		if vector != name {
			out[vector] = append(out[vector], name)
		}
	}
	switch kind {
	case "sandbox":
		for _, x := range c.Sandboxes {
			add(x.Name, x.Vector)
		}
	case "agent":
		for _, x := range c.Agents {
			add(x.Name, x.Vector)
		}
	case "task":
		for _, x := range c.Tasks {
			add(x.Name, x.Vector)
		}
	case "scorer":
		for _, x := range c.Scorers {
			add(x.Name, x.Vector)
		}
	case "suite":
		for _, x := range c.Suites {
			add(x.Name, x.Vector)
		}
	}
	return out
}

// One resolves an name that must name exactly one item of the kind (an agent's
// sandbox, a scorer's agent): the item name, or an error naming the problem.
func (c *Config) One(kind, name string) (string, error) {
	names, ok := c.sets(kind)[name]
	switch {
	case !ok:
		return "", fmt.Errorf("unknown %s %q", kind, name)
	case len(names) != 1:
		return "", fmt.Errorf("%s %q names %d items, need exactly one", kind, name, len(names))
	}
	return names[0], nil
}

// SelectSuites resolves a suite-name filter: empty -> all suites, otherwise the
// named ones (vector or item names, order preserved from the config). unknown
// names are errors.
func (c *Config) SelectSuites(only []string) ([]Suite, error) {
	if len(only) == 0 {
		return c.Suites, nil
	}
	var errs errList
	sets := c.sets("suite")
	want := map[string]bool{}
	for _, name := range only {
		names, ok := sets[name]
		if !ok {
			errs.add("--suite %q: no such suite", name)
			continue
		}
		for _, x := range names {
			want[x] = true
		}
	}
	var out []Suite
	for _, s := range c.Suites {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out, errs.err()
}

// RunSuites is the suites a run selects: the named ones, or with none named
// every suite not marked manual. a config whose suites are all manual has
// none to run by default.
func (c *Config) RunSuites(only []string) ([]string, error) {
	if len(only) > 0 {
		return only, nil
	}
	var out []string
	for _, s := range c.Suites {
		if !s.Manual {
			out = append(out, s.Name)
		}
	}
	if len(out) == 0 && len(c.Suites) > 0 {
		return nil, fmt.Errorf("every suite is manual: name one with -S")
	}
	return out, nil
}

// ManualSuitesWith names the manual suites holding a pair the agent and task
// filters keep: where a filter that missed the default run would land.
func (c *Config) ManualSuitesWith(agents, tasks []string) []string {
	var out []string
	for _, s := range c.Suites {
		if !s.Manual {
			continue
		}
		pairs, err := c.ExpandTestPairs(s.Name)
		if err != nil {
			continue
		}
		if _, err := c.FilterPairs(pairs, agents, tasks); err == nil {
			out = append(out, s.Name)
		}
	}
	return out
}

// ExpandTestPairs crosses each selected suite's agents x tasks. dangling refs
// and duplicate names after expansion within one suite's axis are errors; the
// same pair from two suites is fine (suites may overlap deliberately) and
// deduped.
func (c *Config) ExpandTestPairs(only ...string) ([]TestPair, error) {
	suites, err := c.SelectSuites(only)
	if err != nil {
		return nil, err
	}
	agentSets, taskSets := c.sets("agent"), c.sets("task")

	var out []TestPair
	seen := map[TestPair]bool{}
	var errs errList
	for _, s := range suites {
		agents := expandAxis(&errs, s.Name, "agents", "agent", s.Agents, agentSets)
		tasks := expandAxis(&errs, s.Name, "tasks", "task", s.Tasks, taskSets)
		for _, ag := range agents {
			for _, tk := range tasks {
				p := TestPair{Agent: ag, Task: tk}
				if seen[p] {
					continue
				}
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out, errs.err()
}

// ExpandScorePairs crosses each selected suite's tasks x scorers. same rules
// as ExpandTestPairs.
func (c *Config) ExpandScorePairs(only ...string) ([]ScorePair, error) {
	suites, err := c.SelectSuites(only)
	if err != nil {
		return nil, err
	}
	taskSets, scorerSets := c.sets("task"), c.sets("scorer")

	var out []ScorePair
	seen := map[ScorePair]bool{}
	var errs errList
	for _, s := range suites {
		tasks := expandAxis(&errs, s.Name, "tasks", "task", s.Tasks, taskSets)
		scorers := expandAxis(&errs, s.Name, "scorers", "scorer", s.Scorers, scorerSets)
		for _, tk := range tasks {
			for _, sc := range scorers {
				p := ScorePair{Task: tk, Scorer: sc}
				if seen[p] {
					continue
				}
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out, errs.err()
}

// expandAxis expands one suite axis through the kind's sets: every name must
// be known, and no item may land twice. returns the item names in order (bad
// refs dropped so expansion can keep collecting errors).
func expandAxis(errs *errList, suite, axis, noun string, xs StrList, sets map[string][]string) []string {
	var out []string
	dup := map[string]bool{}
	for _, x := range xs {
		names, ok := sets[x]
		if !ok {
			errs.add("suite %q: unknown %s %q in %s", suite, noun, x, axis)
			continue
		}
		for _, name := range names {
			if dup[name] {
				errs.add("suite %q: duplicate %s %q in %s", suite, noun, name, axis)
				continue
			}
			dup[name] = true
			out = append(out, name)
		}
	}
	return out
}

// FilterPairs narrows expanded test pairs to the given agent and/or task names,
// vector names included (an empty list = no filter on that axis). it errors on
// a filter value that matches no pair (typo guard), or when the combined
// filter selects nothing.
func (c *Config) FilterPairs(pairs []TestPair, agents, tasks []string) ([]TestPair, error) {
	haveA, haveT := map[string]bool{}, map[string]bool{}
	for _, p := range pairs {
		haveA[p.Agent] = true
		haveT[p.Task] = true
	}
	var errs errList
	aset := filterSet(&errs, "agent", agents, c.sets("agent"), haveA)
	tset := filterSet(&errs, "task", tasks, c.sets("task"), haveT)
	if err := errs.err(); err != nil {
		return nil, err
	}

	var out []TestPair
	for _, p := range pairs {
		if len(agents) > 0 && !aset[p.Agent] {
			continue
		}
		if len(tasks) > 0 && !tset[p.Task] {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pairs match the agent/task filter")
	}
	return out, nil
}

// FilterScorePairs narrows expanded score pairs to the given scorer names,
// vector names included (empty = no filter), with FilterPairs' typo guard.
func (c *Config) FilterScorePairs(pairs []ScorePair, scorers []string) ([]ScorePair, error) {
	if len(scorers) == 0 {
		return pairs, nil
	}
	have := map[string]bool{}
	for _, p := range pairs {
		have[p.Scorer] = true
	}
	var errs errList
	set := filterSet(&errs, "scorer", scorers, c.sets("scorer"), have)
	if err := errs.err(); err != nil {
		return nil, err
	}
	var out []ScorePair
	for _, p := range pairs {
		if set[p.Scorer] {
			out = append(out, p)
		}
	}
	return out, nil
}

// filterSet expands the --<kind> filter's names through sets, keeping those
// present in some pair. a filter value that reaches no pair is an error.
func filterSet(errs *errList, kind string, xs []string, sets map[string][]string, have map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		hit := false
		for _, name := range sets[x] {
			if have[name] {
				out[name] = true
				hit = true
			}
		}
		if !hit {
			errs.add("--%s %q: no such %s in the selected suites", kind, x, kind)
		}
	}
	return out
}

// errList accumulates errors into one joined error.
type errList struct{ errs []error }

func (e *errList) add(format string, a ...any) { e.errs = append(e.errs, fmt.Errorf(format, a...)) }

func (e *errList) err() error {
	if len(e.errs) == 0 {
		return nil
	}
	return errors.Join(e.errs...)
}
