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

// noCostHarnesses report no dollar cost (has_cost false): their estimate is
// unknown for that reason, never $0.
var noCostHarnesses = map[string]bool{"codex-cli-keyless": true, "copilot-cli-keyless": true}

// dryPair is one line of the dry run.
type dryPair struct {
	Agent    string   `json:"agent"`
	Task     string   `json:"task"`
	Sandbox  string   `json:"sandbox"`
	Estimate *float64 `json:"estimate_usd"`
	Samples  int      `json:"samples"`
	Unknown  *string  `json:"unknown"`
	Repeat   int      `json:"repeat"`
	Total    *float64 `json:"estimate_total_usd"`
}

// dryTotal is the dry run's last line.
type dryTotal struct {
	Total        bool     `json:"total"`
	Pairs        int      `json:"pairs"`
	Executions   int      `json:"executions"`
	KnownUSD     float64  `json:"known_usd"`
	UnknownPairs int      `json:"unknown_pairs"`
	MaxCostUSD   *float64 `json:"max_cost_usd"`
	Fits         *bool    `json:"fits"`
}

// historyKey is what makes a past pair the same pair.
type historyKey struct{ agent, task, harness, model, effort string }

// dryRun lists pairs with their estimates, to the log or, with JSON, as
// json lines to JSONOut.
func dryRun(cfg *config.Config, opts Options, pairs []config.TestPair, lg logw) error {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	want := map[historyKey]bool{}
	for _, p := range pairs {
		a := agents[p.Agent]
		want[historyKey{p.Agent, p.Task, a.Harness, a.Model, a.Effort}] = true
	}
	hist, err := costHistory(filepath.Join(opts.ConfigDir, runsSubdir), want, lg)
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
		costs := hist[historyKey{p.Agent, p.Task, a.Harness, a.Model, a.Effort}]
		d.Samples = len(costs)
		if len(costs) > 0 {
			m := median(costs)
			t := m * float64(repeat)
			d.Estimate, d.Total = &m, &t
			total.KnownUSD += t
		} else {
			why := "no history"
			if noCostHarnesses[a.Harness] {
				why = "no cost reported"
			}
			d.Unknown = &why
			total.UnknownPairs++
		}
		lines = append(lines, d)
	}
	if opts.MaxCost > 0 {
		c := opts.MaxCost
		fits := total.KnownUSD < c
		total.MaxCostUSD, total.Fits = &c, &fits
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
		lg.info("%s/%s  sandbox %s  %s", d.Agent, d.Task, d.Sandbox, est)
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
		sum += fmt.Sprintf(", %s --max-cost $%.2f", rel, *total.MaxCostUSD)
	}
	lg.info("%s", sum)
	return nil
}

// costHistory is the cost of every past pair matching want, by key, from
// every numbered run dir under base. every pair dir is read, so the rules
// hold for history outside the selection too: a pair dir without metadata
// is skipped; metadata that does not parse is skipped with a warning; any
// other read error stops with its path. no base yet is no history.
func costHistory(base string, want map[historyKey]bool, lg logw) (map[historyKey][]float64, error) {
	names, err := sortedRunNames(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[historyKey][]float64{}
	for _, name := range names {
		run := filepath.Join(base, name)
		agents, err := os.ReadDir(run)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", run, err)
		}
		for _, a := range agents {
			if !a.IsDir() {
				continue
			}
			dir := filepath.Join(run, a.Name())
			tasks, err := os.ReadDir(dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", dir, err)
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
					return nil, fmt.Errorf("%s: %w", path, err)
				}
				var m pairMeta
				if err := json.Unmarshal(b, &m); err != nil {
					lg.warn("skipping %s: %v", relToCwd(path), err)
					continue
				}
				if m.Model == "" || m.Summary == nil || !m.Summary.HasCost {
					continue // from before the model was recorded, or no cost
				}
				k := historyKey{m.Agent, m.Task, m.Harness, m.Model, m.Effort}
				if want[k] {
					out[k] = append(out[k], m.Summary.CostUSD)
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
