package config

import (
	"errors"
	"fmt"
)

// TestPair is one expanded (agent, task) cell of the test matrix.
type TestPair struct {
	Agent string
	Task  string
}

// ScorePair is one expanded (task, scorer) cell of the scorer matrix.
type ScorePair struct {
	Task   string
	Scorer string
}

const wildcard = "*"

// ExpandTestMatrix cross-products every row into (agent, task) pairs. "*"
// resolves to all agents / all tasks. dangling refs and duplicate pairs are errors.
func (c *Config) ExpandTestMatrix() ([]TestPair, error) {
	allAgents := ids(len(c.Agents), func(i int) string { return c.Agents[i].ID })
	agentSet := set(allAgents)
	allTasks := ids(len(c.Tasks), func(i int) string { return c.Tasks[i].ID })
	taskSet := set(allTasks)

	var out []TestPair
	seen := map[string]bool{}
	var errs errList

	for ri, row := range c.TestMatrix {
		rowAgents := resolve(row.Agent, wildcard, allAgents)
		rowTasks := resolve(row.Task, wildcard, allTasks)
		if len(row.Agent) == 0 {
			errs.add("test-matrix row %d: missing agent", ri)
		}
		if len(row.Task) == 0 {
			errs.add("test-matrix row %d: missing task", ri)
		}
		for _, ag := range rowAgents {
			if !agentSet[ag] {
				errs.add("test-matrix row %d: unknown agent %q", ri, ag)
				continue
			}
			for _, tk := range rowTasks {
				if !taskSet[tk] {
					errs.add("test-matrix row %d: unknown task %q", ri, tk)
					continue
				}
				key := ag + "\x00" + tk
				if seen[key] {
					errs.add("test-matrix: duplicate pair %s x %s", ag, tk)
					continue
				}
				seen[key] = true
				out = append(out, TestPair{Agent: ag, Task: tk})
			}
		}
	}
	return out, errs.err()
}

// ExpandScorerMatrix cross-products every row into (task, scorer) pairs. "*"
// resolves to all tasks / all scorers.
func (c *Config) ExpandScorerMatrix() ([]ScorePair, error) {
	allTasks := ids(len(c.Tasks), func(i int) string { return c.Tasks[i].ID })
	taskSet := set(allTasks)
	allScorers := ids(len(c.Scorers), func(i int) string { return c.Scorers[i].ID })
	scorerSet := set(allScorers)

	var out []ScorePair
	seen := map[string]bool{}
	var errs errList

	for ri, row := range c.ScorerMatrix {
		rowTasks := resolve(row.Task, wildcard, allTasks)
		rowScorers := resolve(row.Scorer, wildcard, allScorers)
		if len(row.Task) == 0 {
			errs.add("scorer-matrix row %d: missing task", ri)
		}
		if len(row.Scorer) == 0 {
			errs.add("scorer-matrix row %d: missing scorer", ri)
		}
		for _, tk := range rowTasks {
			if !taskSet[tk] {
				errs.add("scorer-matrix row %d: unknown task %q", ri, tk)
				continue
			}
			for _, sc := range rowScorers {
				if !scorerSet[sc] {
					errs.add("scorer-matrix row %d: unknown scorer %q", ri, sc)
					continue
				}
				key := tk + "\x00" + sc
				if seen[key] {
					errs.add("scorer-matrix: duplicate pair %s x %s", tk, sc)
					continue
				}
				seen[key] = true
				out = append(out, ScorePair{Task: tk, Scorer: sc})
			}
		}
	}
	return out, errs.err()
}

// resolve turns a row field into a concrete list: the "*" sentinel expands to
// all, anything else passes through verbatim.
func resolve(field StrList, star string, all []string) []string {
	if len(field) == 1 && field[0] == star {
		return all
	}
	return field
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
			errs.add("--agent %q: no such agent in the test-matrix", a)
		}
	}
	for _, t := range tasks {
		if !haveT[t] {
			errs.add("--task %q: no such task in the test-matrix", t)
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
