package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// the report over several runs (docs/spec/repeats.md): a pair's score is the
// mean of its per-run means, with sd and n; agents and the overall weigh as
// for one run.

// reportVersionMulti is the document of a report over more than one run.
const reportVersionMulti = 2

type multiDoc struct {
	Version int           `json:"version"`
	Runs    []reportRun   `json:"runs"`
	Overall *float64      `json:"overall"`
	Agents  []reportAgent `json:"agents"`
	Pairs   []multiPair   `json:"pairs"`
}

type multiPair struct {
	Agent   string        `json:"agent"`
	Task    string        `json:"task"`
	Mean    *float64      `json:"mean"`
	SD      *float64      `json:"sd"`
	N       int           `json:"n"`
	Samples []multiSample `json:"samples"`
}

// multiSample is one attempt: a run the pair ran in, its mean null when
// nothing scored it there.
type multiSample struct {
	Run       string             `json:"run"`
	Mean      *float64           `json:"mean"`
	Status    *string            `json:"status"`
	Error     *string            `json:"error"`
	CostUSD   *float64           `json:"cost_usd"`
	DurationS *float64           `json:"duration_s"`
	Scores    map[string]float64 `json:"scores"`
	// EgressEnforced is false when the attempt's egress was advisory (the
	// host driver), null when unknown or no policy ran.
	EgressEnforced *bool `json:"egress_enforced"`
}

// latestRuns is the last n run dirs under base, in run order.
func latestRuns(base string, n int) ([]string, error) {
	if n < 1 {
		return nil, fmt.Errorf("--runs must be at least 1")
	}
	names, err := sortedRunNames(base)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if len(names) < n {
		return nil, fmt.Errorf("asked for the latest %d runs, there are %d under %s", n, len(names), relToCwd(base))
	}
	var out []string
	for _, name := range names[len(names)-n:] {
		out = append(out, filepath.Join(base, name))
	}
	return out, nil
}

// attempts is every pair that ran in runDir: each pair dir, with or without
// metadata, and each scored pair; its metadata nil when there is none or it
// cannot be read.
func attempts(runDir string, rep *ScoreReport, warn io.Writer) []pairRecord {
	recs := readPairs(runDir, rep, warn)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.agent+"/"+r.task] = true
	}
	agents, _ := os.ReadDir(runDir)
	for _, a := range agents {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue // .judge and the like are the run's own, not agents
		}
		tasks, _ := os.ReadDir(filepath.Join(runDir, a.Name()))
		for _, t := range tasks {
			if k := a.Name() + "/" + t.Name(); t.IsDir() && !seen[k] {
				seen[k] = true
				recs = append(recs, pairRecord{agent: a.Name(), task: t.Name()})
			}
		}
	}
	return recs
}

// statusRank orders statuses worst first for a cell's mark; unknown is a
// pair with no readable metadata.
var statusRank = map[string]int{"error": 5, "timeout": 4, "budget": 3, "nonzero": 2, "unknown": 1, "ok": 0}

func recStatus(r pairRecord) string {
	if r.meta == nil {
		return "unknown"
	}
	if _, ok := statusRank[r.meta.Status]; ok {
		return r.meta.Status
	}
	return "unknown"
}

// buildMulti reads runDirs' scores and pairs into the version 2 document,
// with each pair's attempts' statuses for the human pivot.
func buildMulti(runDirs []string, warn io.Writer) (*multiDoc, map[string][]string, error) {
	doc := &multiDoc{Version: reportVersionMulti, Agents: []reportAgent{}, Pairs: []multiPair{}}
	pairs := map[string]*multiPair{}
	statuses := map[string][]string{}
	for _, dir := range runDirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		name := filepath.Base(abs)
		rep, err := readScores(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("run %s is not scored (pats score -N %d)", name, len(runDirs))
		}
		if err != nil {
			return nil, nil, fmt.Errorf("run %s: %w", name, err)
		}
		doc.Runs = append(doc.Runs, reportRun{Name: name, Dir: abs})
		scores := map[string]map[string]float64{}
		for _, s := range rep.Scores {
			k := s.Agent + "/" + s.Task
			if scores[k] == nil {
				scores[k] = map[string]float64{}
			}
			scores[k][s.Scorer] = s.Score
		}
		for _, r := range attempts(dir, rep, warn) {
			k := r.agent + "/" + r.task
			p := pairs[k]
			if p == nil {
				p = &multiPair{Agent: r.agent, Task: r.task, Samples: []multiSample{}}
				pairs[k] = p
			}
			s := multiSample{Run: name, Scores: scores[k]}
			if s.Scores == nil {
				s.Scores = map[string]float64{}
			}
			if m, ok := rep.PerPair[k]; ok {
				s.Mean = &m
			}
			st := recStatus(r)
			statuses[k] = append(statuses[k], st)
			if r.meta != nil {
				status, dur := r.meta.Status, r.meta.DurationS
				s.Status, s.DurationS = &status, &dur
				if r.meta.Error != "" {
					e := r.meta.Error
					s.Error = &e
				}
				if sm := r.meta.Summary; sm != nil && sm.HasCost {
					c := sm.CostUSD
					s.CostUSD = &c
				}
				if eg := r.meta.Egress; eg != nil && eg.Enforced != nil {
					e := *eg.Enforced
					s.EgressEnforced = &e
				}
			}
			p.Samples = append(p.Samples, s)
		}
	}
	// a pair's mean, sd and n over its scored attempts; an agent's mean over
	// its pairs' means; the overall over the agents', as for one run.
	agentSum, agentN := map[string]float64{}, map[string]int{}
	for _, p := range pairs {
		var xs []float64
		for _, s := range p.Samples {
			if s.Mean != nil {
				xs = append(xs, *s.Mean)
			}
		}
		p.N = len(xs)
		if p.N > 0 {
			m := mean(xs)
			p.Mean = &m
			agentSum[p.Agent] += m
			agentN[p.Agent]++
		}
		if p.N > 1 {
			sd := sampleSD(xs)
			p.SD = &sd
		}
		doc.Pairs = append(doc.Pairs, *p)
	}
	sort.Slice(doc.Pairs, func(i, j int) bool {
		a, b := doc.Pairs[i], doc.Pairs[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Task < b.Task
	})
	agentSet := map[string]bool{}
	for _, p := range doc.Pairs {
		agentSet[p.Agent] = true
	}
	osum := 0.0
	for _, name := range sortedSet(agentSet) {
		a := reportAgent{Name: name}
		if agentN[name] > 0 {
			m := agentSum[name] / float64(agentN[name])
			a.Mean = &m
			osum += m
		}
		doc.Agents = append(doc.Agents, a)
	}
	if len(agentN) > 0 {
		o := osum / float64(len(agentN))
		doc.Overall = &o
	}
	return doc, statuses, nil
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sampleSD is the sample standard deviation (n - 1) of at least two values.
func sampleSD(xs []float64) float64 {
	m := mean(xs)
	ss := 0.0
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// reportMulti prints the report over runDirs: the version 2 document with
// asJSON, else the pivot.
func reportMulti(runDirs []string, asJSON bool, out, errOut io.Writer) error {
	doc, statuses, err := buildMulti(runDirs, errOut)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	color := useColor(out)
	logw{out, color, 0}.info("report: the latest %d runs, %s to %s", len(doc.Runs), doc.Runs[0].Name, doc.Runs[len(doc.Runs)-1].Name)
	pivotMulti(out, doc, statuses, color)
	return nil
}

// pivotMulti is the tasks x agents pivot over runs: "mean +-sd", (n) where a
// pair has fewer scored runs than the most any has, the worst status of its
// attempts with the count of non-ok ones, then avg, cost (every attempt's,
// "+" when some attempt reported none) and mean time per attempt.
func pivotMulti(w io.Writer, doc *multiDoc, statuses map[string][]string, color bool) {
	byKey := map[string]multiPair{}
	agentSet, taskSet := map[string]bool{}, map[string]bool{}
	maxN := 0
	for _, p := range doc.Pairs {
		byKey[p.Agent+"/"+p.Task] = p
		agentSet[p.Agent], taskSet[p.Task] = true, true
		maxN = max(maxN, p.N)
	}
	agents, tasks := sortedSet(agentSet), sortedSet(taskSet)
	if len(agents) == 0 {
		return
	}
	markOf := map[string]string{"error": "E", "timeout": "T", "budget": "B", "nonzero": "!", "unknown": "?"}
	cell := func(k string) string {
		p, ok := byKey[k]
		if !ok {
			return "-"
		}
		txt := "-"
		if p.Mean != nil {
			txt = fmt.Sprintf("%.2f", *p.Mean)
			if p.SD != nil {
				txt += fmt.Sprintf(" +-%.2f", *p.SD)
			}
			if p.N < maxN {
				txt += fmt.Sprintf(" (%d)", p.N)
			}
		}
		worst, bad := "ok", 0
		for _, s := range statuses[k] {
			if s != "ok" {
				bad++
			}
			if statusRank[s] > statusRank[worst] {
				worst = s
			}
		}
		if bad > 0 {
			txt += fmt.Sprintf(" %s %d/%d", markOf[worst], bad, len(statuses[k]))
		}
		return txt
	}
	rows := [][]string{append([]string{""}, agents...)}
	for _, t := range tasks {
		r := []string{t}
		for _, a := range agents {
			r = append(r, cell(a+"/"+t))
		}
		rows = append(rows, r)
	}
	avg, cost, tm := []string{"avg"}, []string{"cost"}, []string{"time"}
	for _, a := range agents {
		m := "-"
		for _, ra := range doc.Agents {
			if ra.Name == a && ra.Mean != nil {
				m = fmt.Sprintf("%.2f", *ra.Mean)
			}
		}
		avg = append(avg, m)
		sum, known, missing, secs, nsecs := 0.0, false, false, 0.0, 0
		for _, p := range doc.Pairs {
			if p.Agent != a {
				continue
			}
			for _, s := range p.Samples {
				if s.CostUSD != nil {
					sum += *s.CostUSD
					known = true
				} else {
					missing = true
				}
				if s.DurationS != nil {
					secs += *s.DurationS
					nsecs++
				}
			}
		}
		c := "-"
		if known {
			c = fmt.Sprintf("$%.2f", sum)
			if missing {
				c += "+"
			}
		}
		cost = append(cost, c)
		d := "-"
		if nsecs > 0 {
			d = hms(secs/float64(nsecs)) + " avg"
		}
		tm = append(tm, d)
	}
	if len(tasks) > 1 {
		rows = append(rows, avg)
	}
	rows = append(rows, cost, tm)
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], len(c))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			if i < len(r)-1 {
				fmt.Fprintf(&b, "%-*s", widths[i], c)
			} else {
				b.WriteString(c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	if doc.Overall != nil && len(tasks) > 1 {
		o := fmt.Sprintf("%.2f %s", *doc.Overall, scoreBar(*doc.Overall))
		if color {
			o = scoreColor(*doc.Overall) + o + "\033[0m"
		}
		fmt.Fprintf(w, "%-*s  %s\n", widths[0], "overall", o)
	}
	var advisory []string
	for _, p := range doc.Pairs {
		for _, s := range p.Samples {
			if s.EgressEnforced != nil && !*s.EgressEnforced {
				advisory = append(advisory, p.Agent+"/"+p.Task)
				break
			}
		}
	}
	if len(advisory) > 0 {
		fmt.Fprintf(w, "egress advisory (host driver, unsandboxed; an empty denied list is not clean): %s\n", strings.Join(advisory, ", "))
	}
}
