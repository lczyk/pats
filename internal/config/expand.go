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

// SelectSuites resolves a suite-id filter: empty -> all suites, otherwise the
// named ones (order preserved from the config). unknown ids are errors.
func (c *Config) SelectSuites(only []string) ([]Suite, error) {
	if len(only) == 0 {
		return c.Suites, nil
	}
	byID := map[string]Suite{}
	for _, s := range c.Suites {
		byID[s.ID] = s
	}
	var errs errList
	want := set(only)
	var out []Suite
	for _, s := range c.Suites {
		if want[s.ID] {
			out = append(out, s)
			delete(want, s.ID)
		}
	}
	for _, id := range only {
		if want[id] {
			errs.add("--suite %q: no such suite", id)
		}
	}
	return out, errs.err()
}

// ExpandTestPairs crosses each selected suite's agents x tasks. dangling refs
// and duplicate ids within one suite's list are errors; the same pair from two
// suites is fine (suites may overlap deliberately) and deduped.
func (c *Config) ExpandTestPairs(only ...string) ([]TestPair, error) {
	suites, err := c.SelectSuites(only)
	if err != nil {
		return nil, err
	}
	agentSet := set(ids(len(c.Agents), func(i int) string { return c.Agents[i].ID }))
	taskSet := set(ids(len(c.Tasks), func(i int) string { return c.Tasks[i].ID }))

	var out []TestPair
	seen := map[TestPair]bool{}
	var errs errList
	for _, s := range suites {
		agents := checkAxis(&errs, s.ID, "agents", "agent", s.Agents, agentSet)
		tasks := checkAxis(&errs, s.ID, "tasks", "task", s.Tasks, taskSet)
		for _, ag := range agents {
			for _, tk := range tasks {
				p := TestPair{Agent: ag, Task: tk}
				if seen[p] {
					continue // overlap across suites (e.g. smoke within full)
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
	taskSet := set(ids(len(c.Tasks), func(i int) string { return c.Tasks[i].ID }))
	scorerSet := set(ids(len(c.Scorers), func(i int) string { return c.Scorers[i].ID }))

	var out []ScorePair
	seen := map[ScorePair]bool{}
	var errs errList
	for _, s := range suites {
		tasks := checkAxis(&errs, s.ID, "tasks", "task", s.Tasks, taskSet)
		scorers := checkAxis(&errs, s.ID, "scorers", "scorer", s.Scorers, scorerSet)
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

// checkAxis validates one suite axis: every id must exist in the vector, and
// must not repeat within the list. returns the valid ids (bad ones dropped so
// expansion can keep collecting errors).
func checkAxis(errs *errList, suite, axis, noun string, xs StrList, known map[string]bool) []string {
	var out []string
	dup := map[string]bool{}
	for _, x := range xs {
		if !known[x] {
			errs.add("suite %q: unknown %s %q", suite, noun, x)
			continue
		}
		if dup[x] {
			errs.add("suite %q: duplicate %s %q in %s", suite, noun, x, axis)
			continue
		}
		dup[x] = true
		out = append(out, x)
	}
	return out
}

// FilterPairs narrows expanded test pairs to the given agent and/or task ids
// (an empty list = no filter on that axis). it errors on a filter value that
// matches no pair (typo guard), or when the combined filter selects nothing.
func FilterPairs(pairs []TestPair, agents, tasks []string) ([]TestPair, error) {
	haveA, haveT := map[string]bool{}, map[string]bool{}
	for _, p := range pairs {
		haveA[p.Agent] = true
		haveT[p.Task] = true
	}
	var errs errList
	for _, a := range agents {
		if !haveA[a] {
			errs.add("--agent %q: no such agent in any suite", a)
		}
	}
	for _, t := range tasks {
		if !haveT[t] {
			errs.add("--task %q: no such task in any suite", t)
		}
	}
	if err := errs.err(); err != nil {
		return nil, err
	}

	aset, tset := set(agents), set(tasks)
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

func ids(n int, at func(int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = at(i)
	}
	return out
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
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
