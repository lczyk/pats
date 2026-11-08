package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// reportVersion is `pats report --json`'s format: the interface scripts read.
// a new field keeps it; a field renamed, removed or changing meaning bumps it.
const reportVersion = 1

type reportDoc struct {
	Version int           `json:"version"`
	Run     reportRun     `json:"run"`
	Overall *float64      `json:"overall"`
	Agents  []reportAgent `json:"agents"`
	Pairs   []reportPair  `json:"pairs"`
	Scores  []ScoreResult `json:"scores"`
}

type reportRun struct {
	ID  string `json:"id"`
	Dir string `json:"dir"`
}

type reportAgent struct {
	ID   string   `json:"id"`
	Mean *float64 `json:"mean"`
}

// reportPair is one pair: its mean and what its metadata.json says of the
// run. a field is null when unknown, never zero.
type reportPair struct {
	Agent               string   `json:"agent"`
	Task                string   `json:"task"`
	Mean                *float64 `json:"mean"`
	Status              *string  `json:"status"`
	Error               *string  `json:"error"`
	DurationS           *float64 `json:"duration_s"`
	Turns               *int     `json:"turns"`
	InputTokens         *int     `json:"input_tokens"`
	CacheReadTokens     *int     `json:"cache_read_tokens"`
	CacheCreationTokens *int     `json:"cache_creation_tokens"`
	OutputTokens        *int     `json:"output_tokens"`
	ReasoningTokens     *int     `json:"reasoning_tokens"`
	CostUSD             *float64 `json:"cost_usd"`
	PremiumRequests     *int     `json:"premium_requests"`
	DeniedEgress        []string `json:"denied_egress"`
	BlockedEgress       []string `json:"blocked_egress"`
}

// pairRecord is one pair and its metadata.json: meta nil when there is none
// or it cannot be read (bad says which).
type pairRecord struct {
	agent, task string
	meta        *pairMeta
	bad         bool
}

// readPairs reads every pair's metadata.json under runDir, sorted by agent
// then task, plus a record with no metadata for each scored pair that has
// none (rep may be nil). an unreadable one is kept as bad, and it and any dir
// that cannot be listed are warned about on warn. the dirs are listed, not
// globbed: a run dir's path may hold glob metacharacters.
func readPairs(runDir string, rep *ScoreReport, warn io.Writer) []pairRecord {
	seen := map[string]bool{}
	var out []pairRecord
	warnf := func(format string, args ...any) {
		if warn != nil {
			fmt.Fprintf(warn, "warning: "+format+"\n", args...)
		}
	}
	agents, err := os.ReadDir(runDir)
	if err != nil {
		warnf("%s: %v", relToCwd(runDir), err)
	}
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		dir := filepath.Join(runDir, a.Name())
		tasks, err := os.ReadDir(dir)
		if err != nil {
			warnf("%s: %v", relToCwd(dir), err)
		}
		for _, t := range tasks {
			if !t.IsDir() {
				continue
			}
			m := filepath.Join(runDir, a.Name(), t.Name(), "metadata.json")
			b, err := os.ReadFile(m)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			r := pairRecord{agent: a.Name(), task: t.Name()}
			var pm pairMeta
			if err == nil {
				err = json.Unmarshal(b, &pm)
			}
			if err != nil {
				r.bad = true
				warnf("%s: %v", relToCwd(m), err)
			} else {
				r.meta = &pm
			}
			seen[r.agent+"/"+r.task] = true
			out = append(out, r)
		}
	}
	if rep != nil {
		for _, sc := range rep.Scores {
			if k := sc.Agent + "/" + sc.Task; !seen[k] {
				seen[k] = true
				out = append(out, pairRecord{agent: sc.Agent, task: sc.Task})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].agent != out[j].agent {
			return out[i].agent < out[j].agent
		}
		return out[i].task < out[j].task
	})
	return out
}

// known is n, or nil for 0: a summary keeps a count its log did not carry at
// 0, and a finished agent run never really has none.
func known(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// summaryFields fills p's run fields from pm.
func (p *reportPair) summaryFields(pm *pairMeta) {
	status, dur := pm.Status, pm.DurationS
	p.Status, p.DurationS = &status, &dur
	if pm.Error != "" {
		e := pm.Error
		p.Error = &e
	}
	p.DeniedEgress = append([]string{}, pm.DeniedEgress...)
	p.BlockedEgress = append([]string{}, pm.BlockedEgress...)
	s := pm.Summary
	if s == nil {
		return
	}
	p.Turns = known(s.NumTurns)
	p.InputTokens = known(s.InputTokens)
	p.CacheReadTokens = known(s.CacheReadTokens)
	p.CacheCreationTokens = known(s.CacheCreationTokens)
	p.OutputTokens = known(s.OutputTokens)
	p.ReasoningTokens = known(s.ReasoningTokens)
	p.PremiumRequests = known(s.PremiumRequests)
	if s.HasCost {
		c := s.CostUSD
		p.CostUSD = &c
	}
}

// buildReport joins a run's scores with its pairs' metadata.
func buildReport(runDir string, rep *ScoreReport, warn io.Writer) *reportDoc {
	abs, err := filepath.Abs(runDir)
	if err != nil {
		abs = runDir
	}
	doc := &reportDoc{
		Version: reportVersion,
		Run:     reportRun{ID: filepath.Base(abs), Dir: abs},
		Agents:  []reportAgent{},
		Pairs:   []reportPair{},
		Scores:  append([]ScoreResult{}, rep.Scores...),
	}
	if len(rep.PerAgent) > 0 {
		o := rep.Overall
		doc.Overall = &o
	}

	pairs := map[string]*reportPair{}
	pairOf := func(agent, task string) *reportPair {
		k := agent + "/" + task
		if p := pairs[k]; p != nil {
			return p
		}
		p := &reportPair{Agent: agent, Task: task}
		if m, ok := rep.PerPair[k]; ok {
			p.Mean = &m
		}
		pairs[k] = p
		return p
	}
	for _, r := range readPairs(runDir, rep, warn) {
		p := pairOf(r.agent, r.task)
		if r.meta != nil {
			p.summaryFields(r.meta)
		}
	}

	agents := map[string]bool{}
	for _, p := range pairs {
		doc.Pairs = append(doc.Pairs, *p)
		agents[p.Agent] = true
	}
	sort.Slice(doc.Pairs, func(i, j int) bool {
		a, b := doc.Pairs[i], doc.Pairs[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Task < b.Task
	})
	for id := range agents {
		a := reportAgent{ID: id}
		if m, ok := rep.PerAgent[id]; ok {
			a.Mean = &m
		}
		doc.Agents = append(doc.Agents, a)
	}
	sort.Slice(doc.Agents, func(i, j int) bool { return doc.Agents[i].ID < doc.Agents[j].ID })
	sort.Slice(doc.Scores, func(i, j int) bool {
		a, b := doc.Scores[i], doc.Scores[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.Scorer < b.Scorer
	})
	return doc
}

// Report reprints the scoring report of a past run from its scores.json, or
// with asJSON prints its document: that and nothing else on out, warnings on
// errOut. runArg resolves like `pats score -r`: "" -> latest, else dir or run
// name.
func Report(configDir, runArg string, asJSON bool, out, errOut io.Writer) error {
	runDir, err := resolveRunDir(filepath.Join(configDir, runsSubdir), runArg)
	if err != nil {
		return err
	}
	rep, err := readScores(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no scores for %s (run `pats score` first): %w", relToCwd(runDir), err)
	}
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.MarshalIndent(buildReport(runDir, rep, errOut), "", "  ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	logw{out, useColor(out)}.info("report: %s", relToCwd(runDir))
	report(out, rep, useColor(out))
	statsReport(out, runDir, rep, errOut)
	return nil
}
