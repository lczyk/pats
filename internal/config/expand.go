package config

import (
	"errors"
	"fmt"
)

// TestPair is one expanded (agent, task) cell of a suite.
type TestPair struct {
	Agent string
	Task  string
}

// ScorePair is one expanded (task, scorer) cell of a suite.
type ScorePair struct {
	Task   string
	Scorer string
}

// sets maps every id of a kind to the item ids it names: an item id to
// itself, a vector id to its items in file order. a vector sharing its id
// with its sole item maps once (same set either way); validateNames rejects
// every other overlap.
func (c *Config) sets(kind string) map[string][]string {
	out := map[string][]string{}
	add := func(id, vector string) {
		out[id] = append(out[id], id)
		if vector != id {
			out[vector] = append(out[vector], id)
		}
	}
	switch kind {
	case "sandbox":
		for _, x := range c.Sandboxes {
			add(x.ID, x.Vector)
		}
	case "agent":
		for _, x := range c.Agents {
			add(x.ID, x.Vector)
		}
	case "task":
		for _, x := range c.Tasks {
			add(x.ID, x.Vector)
		}
	case "scorer":
		for _, x := range c.Scorers {
			add(x.ID, x.Vector)
		}
	case "suite":
		for _, x := range c.Suites {
			add(x.ID, x.Vector)
		}
	}
	return out
}

// One resolves an id that must name exactly one item of the kind (an agent's
// sandbox, a scorer's agent): the item id, or an error naming the problem.
func (c *Config) One(kind, id string) (string, error) {
	ids, ok := c.sets(kind)[id]
	switch {
	case !ok:
		return "", fmt.Errorf("unknown %s %q", kind, id)
	case len(ids) != 1:
		return "", fmt.Errorf("%s %q names %d items, need exactly one", kind, id, len(ids))
	}
	return ids[0], nil
}

// SelectSuites resolves a suite-id filter: empty -> all suites, otherwise the
// named ones (vector or item ids, order preserved from the config). unknown
// ids are errors.
func (c *Config) SelectSuites(only []string) ([]Suite, error) {
	if len(only) == 0 {
		return c.Suites, nil
	}
	var errs errList
	sets := c.sets("suite")
	want := map[string]bool{}
	for _, id := range only {
		ids, ok := sets[id]
		if !ok {
			errs.add("--suite %q: no such suite", id)
			continue
		}
		for _, x := range ids {
			want[x] = true
		}
	}
	var out []Suite
	for _, s := range c.Suites {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out, errs.err()
}

// ExpandTestPairs crosses each selected suite's agents x tasks. dangling refs
// and duplicate ids after expansion within one suite's axis are errors; the
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
		agents := expandAxis(&errs, s.ID, "agents", "agent", s.Agents, agentSets)
		tasks := expandAxis(&errs, s.ID, "tasks", "task", s.Tasks, taskSets)
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
		tasks := expandAxis(&errs, s.ID, "tasks", "task", s.Tasks, taskSets)
		scorers := expandAxis(&errs, s.ID, "scorers", "scorer", s.Scorers, scorerSets)
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

// expandAxis expands one suite axis through the kind's sets: every id must
// be known, and no item may land twice. returns the item ids in order (bad
// refs dropped so expansion can keep collecting errors).
func expandAxis(errs *errList, suite, axis, noun string, xs StrList, sets map[string][]string) []string {
	var out []string
	dup := map[string]bool{}
	for _, x := range xs {
		ids, ok := sets[x]
		if !ok {
			errs.add("suite %q: unknown %s %q in %s", suite, noun, x, axis)
			continue
		}
		for _, id := range ids {
			if dup[id] {
				errs.add("suite %q: duplicate %s %q in %s", suite, noun, id, axis)
				continue
			}
			dup[id] = true
			out = append(out, id)
		}
	}
	return out
}

// FilterPairs narrows expanded test pairs to the given agent and/or task ids,
// vector ids included (an empty list = no filter on that axis). it errors on
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

// FilterScorePairs narrows expanded score pairs to the given scorer ids,
// vector ids included (empty = no filter), with FilterPairs' typo guard.
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

// filterSet expands the --<kind> filter's ids through sets, keeping those
// present in some pair. a filter value that reaches no pair is an error.
func filterSet(errs *errList, kind string, xs []string, sets map[string][]string, have map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		hit := false
		for _, id := range sets[x] {
			if have[id] {
				out[id] = true
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
