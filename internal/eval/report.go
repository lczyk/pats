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
	Judges  []reportJudge `json:"judges"`
}

// reportJudge is one judge agent's work in a run: its judgements by outcome,
// and what its samples cost.
type reportJudge struct {
	Name         string   `json:"name"`
	Judgements   int      `json:"judgements"`
	Failed       int      `json:"failed"`
	NA           int      `json:"na"`
	Samples      int      `json:"samples"`
	CostUSD      *float64 `json:"cost_usd"`
	OutputTokens int      `json:"output_tokens"`
	DurationS    float64  `json:"duration_s"`
	// AdvisorySamples are the samples whose egress was advisory (the host
	// driver).
	AdvisorySamples int `json:"advisory_samples"`
}

type reportRun struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type reportAgent struct {
	Name string   `json:"name"`
	Mean *float64 `json:"mean"`
	// FailedPairs is how many of its pairs no judge could grade: they are
	// out of the mean. null when the scores predate the count.
	FailedPairs *int `json:"failed_pairs"`
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
	// EgressEnforced is false when the pair's egress was advisory (the host
	// driver), null when unknown or no policy ran.
	EgressEnforced *bool `json:"egress_enforced"`
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
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue // .judge holds judgements, not pairs
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
	if pm.Egress != nil && pm.Egress.Enforced != nil {
		e := *pm.Egress.Enforced
		p.EgressEnforced = &e
	}
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
		Run:     reportRun{Name: filepath.Base(abs), Dir: abs},
		Agents:  []reportAgent{},
		Pairs:   []reportPair{},
		Scores:  append([]ScoreResult{}, rep.Scores...),
		Judges:  readJudges(runDir, warn),
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
	for name := range agents {
		a := reportAgent{Name: name}
		if m, ok := rep.PerAgent[name]; ok {
			a.Mean = &m
		}
		if rep.FailedPairs != nil {
			n := rep.FailedPairs[name]
			a.FailedPairs = &n
		}
		doc.Agents = append(doc.Agents, a)
	}
	sort.Slice(doc.Agents, func(i, j int) bool { return doc.Agents[i].Name < doc.Agents[j].Name })
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

// readJudges sums each judge agent's work in runDir from .judge/: the
// outcomes from each judgement's verdict.json, the spend from each sample's
// metadata.json. a judgement whose prompt never resolved has a verdict and
// no metadata, so the two are read apart.
func readJudges(runDir string, warn io.Writer) []reportJudge {
	byName := map[string]*reportJudge{}
	judge := func(name string) *reportJudge {
		if byName[name] == nil {
			byName[name] = &reportJudge{Name: name}
		}
		return byName[name]
	}
	subdirs := func(dir string) []string {
		es, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range es {
			if e.IsDir() {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		return out
	}
	// a pair's row is asked once per scorer that judged it.
	rows := map[string]judgeRow{}
	rowOf := func(pairDir string) judgeRow {
		r, ok := rows[pairDir]
		if !ok {
			r = judgeRowOf(pairDir)
			rows[pairDir] = r
		}
		return r
	}
	for _, sc := range subdirs(filepath.Join(runDir, judgeSubdir)) {
		for _, ag := range subdirs(sc) {
			for _, jd := range subdirs(ag) {
				if strings.HasPrefix(filepath.Base(jd), ".") {
					continue // a staged or set-aside set, mid-swap: not a judgement
				}
				// a verdict on a pair no judge would grade is a crash's
				// leftover, not a judgement.
				if rowOf(filepath.Join(runDir, filepath.Base(ag), filepath.Base(jd))) != rowJudge {
					continue
				}
				v, err := readVerdict(jd)
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) && warn != nil {
						fmt.Fprintf(warn, "warning: %v\n", err)
					}
					continue
				}
				j := judge(v.Judge)
				j.Judgements++
				switch {
				case !v.OK:
					j.Failed++
				case v.NA:
					j.NA++
				}
				for _, sd := range subdirs(jd) {
					b, err := os.ReadFile(filepath.Join(sd, "metadata.json"))
					if err != nil {
						continue
					}
					var m pairMeta
					if json.Unmarshal(b, &m) != nil {
						continue
					}
					j.Samples++
					j.DurationS += m.DurationS
					if m.advisoryEgress() {
						j.AdvisorySamples++
					}
					if s := m.Summary; s != nil {
						j.OutputTokens += s.OutputTokens
						if s.HasCost {
							c := s.CostUSD
							if j.CostUSD != nil {
								c += *j.CostUSD
							}
							j.CostUSD = &c
						}
					}
				}
			}
		}
	}
	out := []reportJudge{}
	for _, j := range byName {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

// Report reprints the scoring report of a past run from its scores.json, or
// with asJSON prints its document: that and nothing else on out, warnings on
// errOut. runArg resolves like `pats score -r`: "" -> latest, else dir or run
// name.
func Report(configDir, runArg string, runs int, asJSON bool, out, errOut io.Writer) error {
	if runs > 1 {
		dirs, err := latestRuns(filepath.Join(configDir, runsSubdir), runs)
		if err != nil {
			return err
		}
		return reportMulti(dirs, asJSON, out, errOut)
	}
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
	logw{out, useColor(out), 0}.info("report: %s", relToCwd(runDir))
	report(out, runDir, rep, useColor(out), errOut)
	return nil
}

const scoreBarW = 7

// scoreBar renders "[####---]" with s in [0,1] filling the width; only a 1
// fills it, so a 0.94 never looks like a pass.
func scoreBar(s float64) string {
	filled := max(0, int(s*scoreBarW+0.5))
	if s < 1 {
		filled = min(filled, scoreBarW-1)
	}
	filled = min(filled, scoreBarW)
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", scoreBarW-filled) + "]"
}

// scoreColor picks red/yellow/green ansi by score threshold.
func scoreColor(s float64) string {
	switch {
	case s < 0.5:
		return "\033[31m"
	case s < 0.8:
		return "\033[33m"
	default:
		return "\033[32m"
	}
}

// statusMarks marks a pair that did not end ok; "?" is a pair with scores
// but no readable metadata.json.
var statusMarks = map[string]string{"nonzero": "!", "timeout": "T", "budget": "B", "error": "E"}

// markLegend is the legend's order; it names only the marks a report shows.
var markLegend = []struct{ mark, status string }{{"!", "nonzero"}, {"T", "timeout"}, {"B", "budget"}, {"E", "error"}, {"?", "no metadata"}}

// statusMark is a pair's mark, "" when it ended ok.
func statusMark(r pairRecord) string {
	if r.meta == nil {
		return "?"
	}
	if r.meta.Status == "ok" {
		return ""
	}
	if m, ok := statusMarks[r.meta.Status]; ok {
		return m
	}
	return "?"
}

// hms renders seconds as m:ss, or h:mm:ss from an hour.
func hms(sec float64) string {
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// report prints the human report: a tasks x agents pivot, each cell a
// coloured "0.85 [######-]" and a mark when the pair did not end ok, then
// per-agent avg, cost and time rows and the overall score. the rest is in
// `pats report --json`. warn gets the warnings of unreadable pair dirs.
func report(w io.Writer, runDir string, r *ScoreReport, color bool, warn io.Writer) {
	pairs := readPairs(runDir, r, warn)
	agentSet, taskSet := map[string]bool{}, map[string]bool{}
	marks := map[string]string{}
	cost, costKnown, secs, secsKnown := map[string]float64{}, map[string]bool{}, map[string]float64{}, map[string]bool{}
	for _, p := range pairs {
		agentSet[p.agent], taskSet[p.task] = true, true
		marks[p.agent+"/"+p.task] = statusMark(p)
		if p.meta == nil {
			continue
		}
		secs[p.agent] += p.meta.DurationS
		secsKnown[p.agent] = true
		if s := p.meta.Summary; s != nil && s.HasCost {
			cost[p.agent] += s.CostUSD
			costKnown[p.agent] = true
		}
	}
	for k := range r.PerPair {
		agent, task, _ := strings.Cut(k, "/")
		agentSet[agent], taskSet[task] = true, true
	}
	agents, tasks := sortedSet(agentSet), sortedSet(taskSet)
	if len(agents) == 0 {
		return
	}
	// worst-first: min score across agents, ascending; a task nobody scored
	// last; name breaks ties.
	minOf := func(task string) float64 {
		m := math.Inf(1)
		for _, a := range agents {
			if s, ok := r.PerPair[a+"/"+task]; ok && s < m {
				m = s
			}
		}
		return m
	}
	sort.SliceStable(tasks, func(i, j int) bool { return minOf(tasks[i]) < minOf(tasks[j]) })

	anyMark := false
	for _, m := range marks {
		anyMark = anyMark || m != ""
	}
	labelW := len("overall")
	for _, t := range tasks {
		labelW = max(labelW, len(t))
	}
	cellW := len("0.00 ") + scoreBarW + len("[]")
	if anyMark {
		cellW += len(" X")
	}
	colW := make([]int, len(agents))
	for i, a := range agents {
		colW[i] = max(cellW, len(a))
	}

	fmt.Fprintf(w, "%-*s", labelW, "")
	for i, a := range agents {
		if i < len(agents)-1 {
			fmt.Fprintf(w, "  %-*s", colW[i], a)
		} else {
			fmt.Fprintf(w, "  %s", a)
		}
	}
	fmt.Fprintln(w)

	// each cell is (plain text, ansi colour); colours go on after padding so
	// their bytes don't skew the alignment.
	type cell struct{ text, color string }
	row := func(label string, get func(agent string) []cell) {
		fmt.Fprintf(w, "%-*s", labelW, label)
		for i, a := range agents {
			parts := get(a)
			n := 0
			for j, p := range parts {
				if j > 0 {
					n++
				}
				n += len(p.text)
			}
			fmt.Fprint(w, "  ")
			for j, p := range parts {
				if j > 0 {
					fmt.Fprint(w, " ")
				}
				if color && p.color != "" {
					fmt.Fprint(w, p.color+p.text+"\033[0m")
				} else {
					fmt.Fprint(w, p.text)
				}
			}
			if i < len(agents)-1 {
				fmt.Fprint(w, strings.Repeat(" ", colW[i]-n))
			}
		}
		fmt.Fprintln(w)
	}
	scoreCell := func(s float64, ok bool) cell {
		if !ok {
			return cell{"-", "\033[2m"}
		}
		return cell{fmt.Sprintf("%.2f %s", s, scoreBar(s)), scoreColor(s)}
	}
	markColor := map[string]string{"!": "\033[31m", "E": "\033[31m", "T": "\033[33m", "B": "\033[33m", "?": "\033[2m"}

	for _, t := range tasks {
		row(t, func(a string) []cell {
			s, ok := r.PerPair[a+"/"+t]
			c := []cell{scoreCell(s, ok)}
			if m := marks[a+"/"+t]; m != "" {
				c = append(c, cell{m, markColor[m]})
			}
			return c
		})
	}
	if len(tasks) > 1 {
		row("avg", func(a string) []cell {
			s, ok := r.PerAgent[a]
			return []cell{scoreCell(s, ok)}
		})
	}
	anyFailed := false
	for _, n := range r.FailedPairs {
		anyFailed = anyFailed || n > 0
	}
	if anyFailed {
		// pairs no judge could grade are out of the means above: say how many.
		row("failed", func(a string) []cell {
			if n := r.FailedPairs[a]; n > 0 {
				return []cell{{fmt.Sprintf("%d", n), "\033[31m"}}
			}
			return []cell{{"0", "\033[2m"}}
		})
	}
	row("cost", func(a string) []cell {
		if !costKnown[a] {
			return []cell{{"-", "\033[2m"}}
		}
		return []cell{{fmt.Sprintf("$%.2f", cost[a]), ""}}
	})
	row("time", func(a string) []cell {
		if !secsKnown[a] {
			return []cell{{"-", "\033[2m"}}
		}
		return []cell{{hms(secs[a]), ""}}
	})
	if len(tasks) > 1 && len(r.PerAgent) > 0 {
		overall := fmt.Sprintf("%.2f %s", r.Overall, scoreBar(r.Overall))
		if color {
			overall = scoreColor(r.Overall) + overall + "\033[0m"
		}
		fmt.Fprintf(w, "%-*s  %s\n", labelW, "overall", overall)
	}
	if anyMark {
		var legend []string
		for _, l := range markLegend {
			for _, m := range marks {
				if m == l.mark {
					legend = append(legend, l.mark+" "+l.status)
					break
				}
			}
		}
		fmt.Fprintln(w, strings.Join(legend, "  "))
	}
	var advisory []string
	for _, p := range pairs {
		if p.meta.advisoryEgress() {
			advisory = append(advisory, p.agent+"/"+p.task)
		}
	}
	if len(advisory) > 0 {
		fmt.Fprintf(w, "egress advisory (host driver, unsandboxed; an empty denied list is not clean): %s\n", strings.Join(advisory, ", "))
	}
	for _, j := range readJudges(runDir, warn) {
		line := fmt.Sprintf("judge %s: %d judged, %d failed, %d na, %d samples, %s tok, %s", j.Name,
			j.Judgements, j.Failed, j.NA, j.Samples, humanK(j.OutputTokens), hms(j.DurationS))
		if j.CostUSD != nil {
			line += fmt.Sprintf(", $%.2f", *j.CostUSD)
		}
		if j.AdvisorySamples > 0 {
			line += fmt.Sprintf(", %d with advisory egress", j.AdvisorySamples)
		}
		fmt.Fprintln(w, line)
	}
}
