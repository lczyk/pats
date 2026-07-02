package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/lczyk/pats/internal/config"
)

// ScoreOptions configures the score phase.
type ScoreOptions struct {
	ConfigDir string   // dir holding pats.yaml -- scorer paths resolve against it
	RunDir    string   // explicit run dir, or "" for the latest under .pats/runs
	Jobs      int      // max concurrent scorer cells; 0 -> serial (1), negative -> auto (see resolveJobs)
	Agentic   bool     // also run agent-kind scorers
	Suites    []string // only expand these suites (empty -> all)
	Out       io.Writer
	Color     bool // colour log tags (set internally from Out's tty-ness)

	// noReport skips the report + stats tables (scores.json still written) --
	// set by scoreAll, where per-run tables would just scroll past.
	noReport bool
}

// ScoreReport is the aggregated result of scoring a run.
type ScoreReport struct {
	RunDir   string             `json:"run_dir"`
	Cells    []ScoreCell        `json:"cells"`
	PerPair  map[string]float64 `json:"per_pair"` // "agent/task" -> mean over scorers
	PerAgent map[string]float64 `json:"per_agent"`
	Overall  float64            `json:"overall"`
}

// ScoreCell is one (agent, task, scorer) result.
type ScoreCell struct {
	Agent  string  `json:"agent"`
	Task   string  `json:"task"`
	Scorer string  `json:"scorer"`
	Score  float64 `json:"score"`
}

// Score runs each suite's tasks x scorers over a run's collected outputs and
// aggregates. RunDir "all" scores every run under .pats/runs, oldest first,
// with no report tables printed (the returned report is the last run's).
func Score(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	if opts.RunDir == "all" {
		return scoreAll(cfg, opts)
	}
	// absolute config dir: scorers run with cwd=ConfigDir; their file path +
	// PATS_OUTPUT_DIR must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	opts.Color = useColor(opts.Out)
	lg := logw{opts.Out, opts.Color}
	unlock, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	runDir, err := resolveRunDir(filepath.Join(opts.ConfigDir, runsSubdir), opts.RunDir)
	if err != nil {
		return nil, err
	}
	lg.info("scoring: %s", relToCwd(runDir))

	testPairs, err := cfg.ExpandTestPairs(opts.Suites...)
	if err != nil {
		return nil, err
	}
	scorePairs, err := cfg.ExpandScorePairs(opts.Suites...)
	if err != nil {
		return nil, err
	}
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.ID })
	agentModel := map[string]string{}
	for _, a := range cfg.Agents {
		agentModel[a.ID] = a.ResolvedModel()
	}
	// task -> scorers to run on it.
	byTask := map[string][]config.ScorePair{}
	for _, sp := range scorePairs {
		byTask[sp.Task] = append(byTask[sp.Task], sp)
	}

	// flatten to one job per (pair, scorer) cell so jobs can run concurrently.
	type scoreJob struct {
		agent, task string
		outDir      string
		sc          config.Scorer
	}
	var jobs []scoreJob
	for _, tp := range testPairs {
		outDir := filepath.Join(runDir, tp.Agent, tp.Task)
		if _, err := os.Stat(outDir); err != nil {
			continue // pair didn't run / no output
		}
		for _, sp := range byTask[tp.Task] {
			sc := scorers[sp.Scorer]
			if sc.Kind == "agent" && !opts.Agentic {
				continue // agentic scorers gated behind --agentic
			}
			jobs = append(jobs, scoreJob{tp.Agent, tp.Task, outDir, sc})
		}
	}

	njobs := resolveJobs(opts.Jobs)
	var bar *progress
	if isProgressTTY(opts.Out) {
		labelW := 0
		for _, j := range jobs {
			if w := len(j.agent) + len(" x ") + len(j.task) + len(" x ") + len(j.sc.ID); w > labelW {
				labelW = w
			}
		}
		bar = newProgress(opts.Out, len(jobs), labelW)
		defer bar.close()
	} else if njobs > 1 {
		lg.info("scoring %d cells, up to %d in parallel", len(jobs), njobs)
	}

	// same bounded-worker-pool pattern as Run: buffered per-job output emitted
	// atomically on completion so concurrent progress/log writes don't interleave.
	sem := make(chan struct{}, njobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var cells []ScoreCell
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(j scoreJob) {
			defer wg.Done()
			defer func() { <-sem }()

			label := j.agent + " x " + j.task + " x " + j.sc.ID
			po := opts
			var buf bytes.Buffer
			if bar != nil || njobs > 1 {
				po.Out = &buf
			}
			jlg := logw{po.Out, opts.Color}
			if bar != nil {
				bar.start(label, &pairStat{}, "", false)
			}

			score, serr := runScorer(po, j.sc, j.outDir, j.agent, j.task, agentModel[j.agent])
			switch {
			case errors.Is(serr, errScorerNA):
				jlg.info("[%s x %s] %s = n/a", j.agent, j.task, j.sc.ID)
			case serr != nil:
				jlg.error("[%s x %s] %s: %v", j.agent, j.task, j.sc.ID, serr)
			default:
				jlg.info("[%s x %s] %s = %.4f", j.agent, j.task, j.sc.ID, score)
				mu.Lock()
				cells = append(cells, ScoreCell{j.agent, j.task, j.sc.ID, score})
				mu.Unlock()
			}

			switch {
			case bar != nil:
				bar.finish(label, buf.String())
			case njobs > 1:
				mu.Lock()
				opts.Out.Write(buf.Bytes())
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()

	rep := aggregate(runDir, cells, testPairs)
	if !opts.noReport {
		report(opts.Out, rep, opts.Color)
	}
	if err := writeJSON(filepath.Join(runDir, "scores.json"), rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// Report reprints the scoring report of a past run from its scores.json.
// runArg resolves like `pats score -r`: "" -> latest, else dir or run name.
func Report(configDir, runArg string, out io.Writer) error {
	runDir, err := resolveRunDir(filepath.Join(configDir, runsSubdir), runArg)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(runDir, "scores.json"))
	if err != nil {
		return fmt.Errorf("no scores for %s (run `pats score` first): %w", relToCwd(runDir), err)
	}
	var rep ScoreReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("parse scores.json: %w", err)
	}
	logw{out, useColor(out)}.info("report: %s", relToCwd(runDir))
	report(out, &rep, useColor(out))
	return nil
}

// errScorerNA is the silent-skip signal: a scorer printed "na", meaning it
// doesn't apply to this cell. dropped from aggregation, not logged as an error.
var errScorerNA = errors.New("scorer reported n/a")

// runScorer runs one scorer over an output dir and returns its [0,1] score.
// file scorers exec directly on the host (trusted user scripts); the shebang
// picks the interpreter. agent scorers pending.
func runScorer(opts ScoreOptions, sc config.Scorer, outDir, agent, task, model string) (float64, error) {
	switch sc.Kind {
	case "":
		cmd, err := commandFor(opts.ConfigDir, sc.ExecFile(), map[string]string{
			"PATS_OUTPUT_DIR": outDir,
			"PATS_AGENT_ID":   agent,
			"PATS_TASK_ID":    task,
			"PATS_SCORER_ID":  sc.ID,
			"PATS_MODEL":      model,
		})
		if err != nil {
			return 0, err
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil { // non-zero exit = failure
			return 0, fmt.Errorf("run: %w%s", err, stderrTail(&stderr))
		}
		return parseScore(stdout.String())
	case "agent":
		return 0, fmt.Errorf("agent scorer not implemented yet")
	default:
		return 0, fmt.Errorf("unknown scorer kind %q", sc.Kind)
	}
}

// parseScore reads a scorer's verdict from the first non-empty stdout line: a
// [0,1] float, or "na" -> errScorerNA (a silent skip).
func parseScore(s string) (float64, error) {
	first := ""
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return 0, fmt.Errorf("no output")
	}
	if strings.EqualFold(first, "na") {
		return 0, errScorerNA
	}
	f, err := strconv.ParseFloat(first, 64)
	if err != nil {
		return 0, fmt.Errorf("output %q is not a float or na", first)
	}
	if math.IsNaN(f) || f < 0 || f > 1 {
		return 0, fmt.Errorf("score %v out of [0,1]", f)
	}
	return f, nil
}

func aggregate(runDir string, cells []ScoreCell, testPairs []config.TestPair) *ScoreReport {
	// per (agent,task): mean over scorers.
	type acc struct {
		n   int
		sum float64
	}
	pair := map[string]*acc{}
	for _, c := range cells {
		k := c.Agent + "/" + c.Task
		a := pair[k]
		if a == nil {
			a = &acc{}
			pair[k] = a
		}
		a.n++
		a.sum += c.Score
	}
	perPair := map[string]float64{}
	for k, a := range pair {
		if a.n > 0 {
			perPair[k] = a.sum / float64(a.n)
		}
	}

	// per agent: mean over its tasks.
	agent := map[string]*acc{}
	for _, tp := range testPairs {
		k := tp.Agent + "/" + tp.Task
		ts, ok := perPair[k]
		if !ok {
			continue
		}
		a := agent[tp.Agent]
		if a == nil {
			a = &acc{}
			agent[tp.Agent] = a
		}
		a.n++
		a.sum += ts
	}
	perAgent := map[string]float64{}
	var osum float64
	for id, a := range agent {
		if a.n > 0 {
			perAgent[id] = a.sum / float64(a.n)
			osum += perAgent[id]
		}
	}
	overall := 0.0
	if len(perAgent) > 0 {
		overall = osum / float64(len(perAgent))
	}
	return &ScoreReport{
		RunDir: runDir, Cells: cells,
		PerPair: perPair, PerAgent: perAgent, Overall: overall,
	}
}

const scoreBarW = 7

// scoreBar renders "[####---]" with s in [0,1] filling the width.
func scoreBar(s float64) string {
	filled := min(scoreBarW, max(0, int(s*scoreBarW+0.5)))
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

// report prints a tasks x agents pivot table -- each cell a coloured
// "0.85 [######-]" -- with per-agent averages, the overall score, and a
// per-scorer breakdown for imperfect pairs (worst first).
func report(w io.Writer, r *ScoreReport, color bool) {
	agents := sortedKeys(r.PerAgent)
	taskSet := map[string]bool{}
	for k := range r.PerPair {
		_, task, _ := strings.Cut(k, "/")
		taskSet[task] = true
	}
	tasks := make([]string, 0, len(taskSet))
	for t := range taskSet {
		tasks = append(tasks, t)
	}
	// worst-first: min cell across agents, ascending; name breaks ties.
	minOf := func(task string) float64 {
		m := math.Inf(1)
		for _, a := range agents {
			if s, ok := r.PerPair[a+"/"+task]; ok && s < m {
				m = s
			}
		}
		return m
	}
	sort.Slice(tasks, func(i, j int) bool {
		mi, mj := minOf(tasks[i]), minOf(tasks[j])
		if mi != mj {
			return mi < mj
		}
		return tasks[i] < tasks[j]
	})

	labelW := len("overall")
	for _, t := range tasks {
		labelW = max(labelW, len(t))
	}
	cellW := len("0.00 ") + scoreBarW + len("[]")
	colW := make([]int, len(agents))
	for i, a := range agents {
		colW[i] = max(cellW, len(a))
	}

	fmt.Fprintf(w, "%-*s", labelW, "")
	for i, a := range agents {
		fmt.Fprintf(w, "  %*s", colW[i], a)
	}
	fmt.Fprintln(w)

	// cell colours applied after padding so ansi bytes don't skew alignment.
	row := func(label string, get func(agent string) (float64, bool)) {
		fmt.Fprintf(w, "%-*s", labelW, label)
		for i, a := range agents {
			s, ok := get(a)
			cell, c := "-", "\033[2m" // missing -> dim dash
			if ok {
				cell, c = fmt.Sprintf("%.2f %s", s, scoreBar(s)), scoreColor(s)
			}
			pad := strings.Repeat(" ", colW[i]-len(cell))
			if color {
				cell = c + cell + "\033[0m"
			}
			fmt.Fprintf(w, "  %s%s", pad, cell)
		}
		fmt.Fprintln(w)
	}

	for _, t := range tasks {
		row(t, func(a string) (float64, bool) { s, ok := r.PerPair[a+"/"+t]; return s, ok })
	}
	row("avg", func(a string) (float64, bool) { s, ok := r.PerAgent[a]; return s, ok })

	overall := fmt.Sprintf("%.2f %s", r.Overall, scoreBar(r.Overall))
	if color {
		overall = scoreColor(r.Overall) + overall + "\033[0m"
	}
	fmt.Fprintf(w, "%-*s  %s\n", labelW, "overall", overall)

	// per-scorer breakdown, imperfect pairs only, worst first.
	var imperfect []string
	for _, k := range sortedKeys(r.PerPair) {
		if r.PerPair[k] < 0.999 {
			imperfect = append(imperfect, k)
		}
	}
	if len(imperfect) == 0 {
		return
	}
	sort.Slice(imperfect, func(i, j int) bool {
		si, sj := r.PerPair[imperfect[i]], r.PerPair[imperfect[j]]
		if si != sj {
			return si < sj
		}
		return imperfect[i] < imperfect[j]
	})
	fmt.Fprintln(w, "\nimperfect pairs (scorer breakdown):")
	kw := 0
	for _, k := range imperfect {
		kw = max(kw, len(k))
	}
	for _, k := range imperfect {
		agent, task, _ := strings.Cut(k, "/")
		var parts []string
		for _, c := range r.Cells {
			if c.Agent == agent && c.Task == task {
				p := fmt.Sprintf("%s=%.2f", c.Scorer, c.Score)
				if color {
					p = scoreColor(c.Score) + p + "\033[0m"
				}
				parts = append(parts, p)
			}
		}
		fmt.Fprintf(w, "  %-*s  %s\n", kw, k, strings.Join(parts, ", "))
	}
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// scoreAll scores every run under .pats/runs in order. one run failing stops
// the loop -- later runs would silently miss from the output otherwise.
func scoreAll(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	configDir, err := filepath.Abs(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(configDir, runsSubdir)
	names, err := sortedRunNames(base)
	if err != nil || len(names) == 0 {
		return nil, fmt.Errorf("no runs found under %s", base)
	}
	var rep *ScoreReport
	for _, name := range names {
		o := opts
		o.RunDir = filepath.Join(base, name)
		o.noReport = true
		if rep, err = Score(cfg, o); err != nil {
			return nil, fmt.Errorf("run %s: %w", name, err)
		}
	}
	return rep, nil
}

// resolveRunDir maps the -r argument to a run dir: "" -> the latest run, an
// integer -> by run number (see resolveRunNumber), an existing dir (name or
// path) -> itself, anything else -> a run whose friendly words match (so
// `-r fluffy-bunny` works). ambiguous words are an error.
func resolveRunDir(base, arg string) (string, error) {
	if arg == "" {
		return latestRunDir(base)
	}
	if n, err := strconv.Atoi(arg); err == nil {
		return resolveRunNumber(base, n)
	}
	for _, dir := range []string{arg, filepath.Join(base, arg)} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
	}
	names, err := sortedRunNames(base)
	if err != nil {
		return "", fmt.Errorf("run %q not found under %s: %w", arg, base, err)
	}
	var matches []string
	for _, name := range names {
		if strings.HasSuffix(name, "-"+arg) {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 1:
		return filepath.Join(base, matches[0]), nil
	case 0:
		return "", fmt.Errorf("run %q not found under %s (not a dir, no name matches)", arg, base)
	default:
		return "", fmt.Errorf("run name %q is ambiguous: %s", arg, strings.Join(matches, ", "))
	}
}

// resolveRunNumber maps an integer -r to a run dir. positive n is a run
// number (the <nnn> prefix, so -r 1 == -r 001); zero and negative index from
// the latest backwards (0 -> latest, -1 -> second to last, ...).
func resolveRunNumber(base string, n int) (string, error) {
	names, err := sortedRunNames(base)
	if err != nil || len(names) == 0 {
		return "", fmt.Errorf("no runs found under %s", base)
	}
	if n > 0 {
		for _, name := range names {
			if _, rn, ok := splitRunName(name); ok && rn == n {
				return filepath.Join(base, name), nil
			}
		}
		return "", fmt.Errorf("run number %d not found under %s", n, base)
	}
	i := len(names) - 1 + n
	if i < 0 {
		return "", fmt.Errorf("run %d is out of range: only %d runs under %s", n, len(names), base)
	}
	return filepath.Join(base, names[i]), nil
}

// latestRunDir returns the highest-sorted run dir under base.
func latestRunDir(base string) (string, error) {
	names, err := sortedRunNames(base)
	if err != nil {
		return "", fmt.Errorf("no runs found under %s (run `pats run` first): %w", base, err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no runs found under %s", base)
	}
	return filepath.Join(base, names[len(names)-1]), nil
}

// sortedRunNames lists the run dirs under base, sorted by (date, numeric
// suffix) so 20260621-10 beats 20260621-2.
func sortedRunNames(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		di, ni, _ := splitRunName(names[i])
		dj, nj, _ := splitRunName(names[j])
		if di != dj {
			return di < dj
		}
		return ni < nj
	})
	return names, nil
}

// splitRunName parses a run dir name, <nnn>-<yyyymmdd>-<adj>-<noun>. non-run
// entries (e.g. the "latest" symlink) come back ok=false.
func splitRunName(name string) (date string, n int, ok bool) {
	first, rest, _ := strings.Cut(name, "-")
	n, err := strconv.Atoi(first)
	if err != nil {
		return "", 0, false
	}
	date, _, _ = strings.Cut(rest, "-")
	return date, n, true
}
