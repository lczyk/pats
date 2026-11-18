package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/lczyk/pats/internal/config"
)

// the dry run (docs/spec/run-dry-run.md): the pairs a run would run, each
// with an estimate from what the same pair cost before.

// dryPair is one line of the dry run.
type dryPair struct {
	Agent    string   `json:"agent"`
	Task     string   `json:"task"`
	Sandbox  string   `json:"sandbox"`
	Steps    string   `json:"steps,omitempty"` // the step sandbox the task's steps run in
	Estimate *float64 `json:"estimate_usd"`
	Samples  int      `json:"samples"`
	Unknown  *string  `json:"unknown"`
	Repeat   int      `json:"repeat"`
	Total    *float64 `json:"estimate_total_usd"`
}

// dryTotal is the dry run's last line.
type dryTotal struct {
	Total        bool       `json:"total"`
	Pairs        int        `json:"pairs"`
	Executions   int        `json:"executions"`
	KnownUSD     float64    `json:"known_usd"`
	UnknownPairs int        `json:"unknown_pairs"`
	Budget       *dryBudget `json:"budget"`
	Fits         *bool      `json:"fits"` // the estimate is under the cost cap; null without one
}

// dryBudget is the caps --budget gave, each absent when not given.
type dryBudget struct {
	CostUSD *float64 `json:"cost_usd,omitempty"`
	Tokens  *int64   `json:"tokens,omitempty"`
	Tools   *int64   `json:"tools,omitempty"`
	Turns   *int64   `json:"turns,omitempty"`
	TimeS   *float64 `json:"time_s,omitempty"`
}

func dryBudgetOf(b config.Budget) *dryBudget {
	if b.IsZero() {
		return nil
	}
	var d dryBudget
	if b.Cost > 0 {
		d.CostUSD = &b.Cost
	}
	if b.Tokens > 0 {
		d.Tokens = &b.Tokens
	}
	if b.Tools > 0 {
		d.Tools = &b.Tools
	}
	if b.Turns > 0 {
		d.Turns = &b.Turns
	}
	if b.Time > 0 {
		s := b.Time.Seconds()
		d.TimeS = &s
	}
	return &d
}

// historyKey is what makes a past pair the same pair.
type historyKey struct{ agent, task, harness, model, effort string }

func keyOf(p config.TestPair, a config.Agent) historyKey {
	return historyKey{p.Agent, p.Task, a.Harness, a.Model, a.Effort}
}

// dryRun lists pairs with their estimates, to the log or, with JSON, as
// json lines to JSONOut.
func dryRun(cfg *config.Config, opts Options, pairs []config.TestPair, lg logw) error {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	want := map[historyKey]bool{}
	for _, p := range pairs {
		a := agents[p.Agent]
		want[keyOf(p, a)] = true
	}
	hist, err := readHistory(filepath.Join(opts.ConfigDir, runsSubdir), want, true, lg)
	if err != nil {
		return err
	}
	repeat := max(opts.Repeat, 1)
	var lines []dryPair
	total := dryTotal{Total: true, Pairs: len(pairs), Executions: len(pairs) * repeat}
	for _, p := range pairs {
		a := agents[p.Agent]
		sb, _ := cfg.ResolveSandbox(a)
		d := dryPair{Agent: p.Agent, Task: p.Task, Sandbox: sb, Repeat: repeat}
		for _, t := range cfg.Tasks {
			if t.Name == p.Task && t.StepSandbox != "" {
				d.Steps, _ = cfg.One("sandbox", t.StepSandbox)
			}
		}
		var costs []float64
		if h := hist[keyOf(p, a)]; h != nil {
			costs = h.costs
		}
		d.Samples = len(costs)
		if len(costs) > 0 {
			m := median(costs)
			t := m * float64(repeat)
			d.Estimate, d.Total = &m, &t
			total.KnownUSD += t
		} else {
			// a harness that reports no dollar cost leaves no history: its
			// estimate is unknown, never $0.
			why := "no history"
			d.Unknown = &why
			total.UnknownPairs++
		}
		lines = append(lines, d)
	}
	total.Budget = dryBudgetOf(opts.Budget)
	if c := opts.Budget.Cost; c > 0 {
		fits := total.KnownUSD < c
		total.Fits = &fits
	}
	if opts.JSON {
		enc := json.NewEncoder(opts.JSONOut)
		for _, d := range lines {
			if err := enc.Encode(d); err != nil {
				return err
			}
		}
		return enc.Encode(total)
	}
	for _, d := range lines {
		est := "est unknown (" + derefStr(d.Unknown) + ")"
		if d.Estimate != nil {
			est = fmt.Sprintf("est $%.2f", *d.Estimate)
			if repeat > 1 {
				est += fmt.Sprintf(" x%d = $%.2f", repeat, *d.Total)
			}
			est += fmt.Sprintf(" (median of %d)", d.Samples)
		}
		where := "sandbox " + d.Sandbox
		if d.Steps != "" {
			where += "  steps " + d.Steps
		}
		lg.info("%s/%s  %s  %s", d.Agent, d.Task, where, est)
	}
	sum := fmt.Sprintf("%d pair(s)", total.Pairs)
	if repeat > 1 {
		sum += fmt.Sprintf(" x%d", repeat)
	}
	sum += fmt.Sprintf(", est $%.2f", total.KnownUSD)
	if total.UnknownPairs > 0 {
		sum += fmt.Sprintf(" + %d unknown", total.UnknownPairs)
	}
	if total.Fits != nil {
		rel := "under"
		if !*total.Fits {
			rel = "not under"
		}
		sum += fmt.Sprintf(", %s --budget $%.2f", rel, opts.Budget.Cost)
	}
	if rest := (config.Budget{Tokens: opts.Budget.Tokens, Tools: opts.Budget.Tools, Turns: opts.Budget.Turns, Time: opts.Budget.Time}); !rest.IsZero() {
		sum += ", --budget also caps " + rest.String() + " (no estimate)"
	}
	lg.info("%s", sum)
	return nil
}

// pastPairs is what past pairs matching a key ran: the cost of those that
// reported one, and the agent's duration of those that ran it (any status
// but error), in seconds.
type pastPairs struct {
	costs, durations []float64
}

// readHistory is every past pair matching want, by key, from every numbered
// run dir under base. every pair dir is read, so the rules hold for history
// outside the selection too: a pair dir without metadata is skipped. strict
// is the dry run's: metadata that does not parse is skipped with a warning,
// and any other read error stops with its path. otherwise, as the
// dispatcher reads it, both are skipped without a word. no base yet is no
// history.
func readHistory(base string, want map[historyKey]bool, strict bool, lg logw) (map[historyKey]*pastPairs, error) {
	names, err := sortedRunNames(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if !strict {
			return nil, nil
		}
		return nil, err
	}
	out := map[historyKey]*pastPairs{}
	readErr := func(path string, err error) error {
		if !strict {
			return nil
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range names {
		run := filepath.Join(base, name)
		agents, err := os.ReadDir(run)
		if err != nil {
			if err := readErr(run, err); err != nil {
				return nil, err
			}
			continue
		}
		for _, a := range agents {
			if !a.IsDir() {
				continue
			}
			dir := filepath.Join(run, a.Name())
			tasks, err := os.ReadDir(dir)
			if err != nil {
				if err := readErr(dir, err); err != nil {
					return nil, err
				}
				continue
			}
			for _, t := range tasks {
				if !t.IsDir() {
					continue
				}
				path := filepath.Join(dir, t.Name(), "metadata.json")
				b, err := os.ReadFile(path)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					if err := readErr(path, err); err != nil {
						return nil, err
					}
					continue
				}
				var m pairMeta
				if err := json.Unmarshal(b, &m); err != nil {
					if strict {
						lg.warn("skipping %s: %v", relToCwd(path), err)
					}
					continue
				}
				k := historyKey{m.Agent, m.Task, m.Harness, m.Model, m.Effort}
				if !want[k] {
					continue
				}
				p := out[k]
				if p == nil {
					p = &pastPairs{}
					out[k] = p
				}
				if m.Summary != nil && m.Summary.HasCost {
					p.costs = append(p.costs, m.Summary.CostUSD)
				}
				if m.Status != "error" && m.Status != "" {
					p.durations = append(p.durations, m.DurationS)
				}
			}
		}
	}
	return out, nil
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
