package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
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
	Jobs      int      // max concurrent scores; 0 -> serial (1), negative -> auto (see resolveJobs)
	Judge     bool     // also run agent scorers
	Suites    []string // only expand these suites (empty -> all)
	Agents    []string // filter the scored pairs to these agents (empty -> all)
	Tasks     []string // filter the scored pairs to these tasks (empty -> all)
	Scorers   []string // only run these scorers (empty -> all)
	Out       io.Writer
	Color     bool // colour log tags (set internally from Out's tty-ness)

	// noReport skips the report + stats tables (scores.json still written) --
	// set by scoreAll, where per-run tables would just scroll past.
	noReport bool
}

// scoresVersion is scores.json's own format. a file of any other version was
// written by another pats: it reads as unscored and is never merged into.
const scoresVersion = 2

// ScoreReport is the aggregated result of scoring a run, kept as scores.json.
// it is pats's own state; `pats report --json` is the interface (report.go).
type ScoreReport struct {
	Version  int                `json:"version"`
	RunDir   string             `json:"run_dir"`
	Scores   []ScoreResult      `json:"scores"`
	PerPair  map[string]float64 `json:"per_pair"` // "agent/task" -> mean over scorers
	PerAgent map[string]float64 `json:"per_agent"`
	Overall  float64            `json:"overall"`
}

// ScoreResult is one scorer's score for one pair.
type ScoreResult struct {
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
	pairFilter := len(opts.Agents) > 0 || len(opts.Tasks) > 0
	if pairFilter {
		if testPairs, err = cfg.FilterPairs(testPairs, opts.Agents, opts.Tasks); err != nil {
			return nil, err
		}
	}
	if scorePairs, err = cfg.FilterScorePairs(scorePairs, opts.Scorers); err != nil {
		return nil, err
	}
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.ID })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.ID })
	agents := map[string]agentInfo{}
	for _, a := range cfg.Agents {
		agents[a.ID] = agentInfo{model: a.Model, harness: a.Harness}
	}
	// task -> scorers to run on it.
	byTask := map[string][]config.ScorePair{}
	for _, sp := range scorePairs {
		byTask[sp.Task] = append(byTask[sp.Task], sp)
	}

	// flatten to one job per (pair, scorer) score so jobs can run concurrently.
	type scoreJob struct {
		agent, task string
		outDir      string
		taskDir     string
		sc          config.Scorer
	}
	var jobs []scoreJob
	gated := 0
	for _, tp := range testPairs {
		outDir := filepath.Join(runDir, tp.Agent, tp.Task)
		if _, err := os.Stat(outDir); err != nil {
			continue // pair didn't run / no output
		}
		for _, sp := range byTask[tp.Task] {
			sc := scorers[sp.Scorer]
			if sc.IsAgent() && !opts.Judge {
				gated++
				continue // agent scorers gated behind --judge
			}
			jobs = append(jobs, scoreJob{tp.Agent, tp.Task, outDir, filepath.Join(opts.ConfigDir, tasks[tp.Task].Dir), sc})
		}
	}
	// a narrowed score that reaches nothing must not rewrite scores.json empty.
	narrowed := len(opts.Suites) > 0 || pairFilter || len(opts.Scorers) > 0
	if len(jobs) == 0 && narrowed {
		hint := ""
		if gated > 0 {
			hint = fmt.Sprintf(" (%d agent score(s) need --judge)", gated)
		}
		return nil, fmt.Errorf("%w: no pair with output in this run has a selected scorer%s", errNothingToScore, hint)
	}

	njobs := resolveJobs(opts.Jobs, runtime.NumCPU)
	var bar *progress
	if isProgressTTY(opts.Out) {
		labelW := 0
		for _, j := range jobs {
			if w := len(j.agent) + len("/") + len(j.task) + len("/") + len(j.sc.ID); w > labelW {
				labelW = w
			}
		}
		bar = newProgress(opts.Out, len(jobs), labelW)
		defer bar.close()
	} else if njobs > 1 {
		lg.info("computing %d scores, up to %d in parallel", len(jobs), njobs)
	}

	// same bounded-worker-pool pattern as Run: buffered per-job output emitted
	// atomically on completion so concurrent progress/log writes don't interleave.
	ctx := context.Background()
	sem := make(chan struct{}, njobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []ScoreResult
	for _, j := range jobs {
		if ctx.Err() != nil {
			if bar != nil {
				bar.log(lg.line(lvWarn, "interrupted -- stopping"))
			} else {
				lg.warn("interrupted -- stopping")
			}
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(j scoreJob) {
			defer wg.Done()
			defer func() { <-sem }()

			label := j.agent + "/" + j.task + " " + j.sc.ID
			po := opts
			var buf bytes.Buffer
			if bar != nil || njobs > 1 {
				po.Out = &buf
			}
			jlg := logw{po.Out, opts.Color}
			if bar != nil {
				bar.start(label, &pairStat{}, "", false)
			}

			score, serr := runScorer(ctx, po, j.sc, runDir, j.outDir, j.taskDir, j.agent, j.task, agents[j.agent])
			switch {
			case errors.Is(serr, errScorerNA):
				jlg.info("%s/%s %s = n/a", j.agent, j.task, j.sc.ID)
			case serr != nil:
				jlg.error("%s/%s %s: %v", j.agent, j.task, j.sc.ID, serr)
			default:
				jlg.info("%s/%s %s = %.4f", j.agent, j.task, j.sc.ID, score)
				mu.Lock()
				results = append(results, ScoreResult{j.agent, j.task, j.sc.ID, score})
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

	var rep *ScoreReport
	if narrowed {
		scope := map[ScoreResult]bool{}
		for _, j := range jobs {
			scope[ScoreResult{Agent: j.agent, Task: j.task, Scorer: j.sc.ID}] = true
		}
		if rep, err = mergeScores(cfg, runDir, results, scope); err != nil {
			return nil, err
		}
	} else {
		rep = aggregate(runDir, results, testPairs)
	}
	if !opts.noReport {
		report(opts.Out, runDir, rep, opts.Color, opts.Out)
	}
	if err := writeJSON(filepath.Join(runDir, "scores.json"), rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// errOldScores: a scores.json of another format, from another pats.
var errOldScores = errors.New("was scored by an older pats")

// readScores reads a run's scores.json. a missing file is fs.ErrNotExist; one
// of another version is errOldScores, never decoded as this one.
func readScores(runDir string) (*ScoreReport, error) {
	data, err := os.ReadFile(filepath.Join(runDir, "scores.json"))
	if err != nil {
		return nil, err
	}
	var rep ScoreReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("parse scores.json: %w", err)
	}
	if rep.Version != scoresVersion {
		return nil, fmt.Errorf("%s %w: run `pats score -r %s`", relToCwd(runDir), errOldScores, filepath.Base(runDir))
	}
	return &rep, nil
}

// errScorerNA is the silent-skip signal: a scorer printed "na", meaning it
// doesn't apply to this pair. dropped from aggregation, not logged as an error.
var errScorerNA = errors.New("scorer reported n/a")

// errNothingToScore: a narrowed selection reaches no score in a run.
var errNothingToScore = errors.New("nothing to score")

// agentInfo carries the per-agent fields a scorer's env needs, keyed by agent
// id in Score's agents map -- one small struct beats a second parallel map
// alongside runScorer's already-long argument list.
type agentInfo struct {
	model   string
	harness string
}

// runScorer runs one scorer over an output dir and returns its [0,1] score.
// script scorers run on the host (trusted user scripts) in the dir they were
// defined in. agent scorers pending (they will run under ctx).
func runScorer(ctx context.Context, opts ScoreOptions, sc config.Scorer, runDir, outDir, taskDir, agent, task string, info agentInfo) (float64, error) {
	if sc.IsAgent() {
		return 0, fmt.Errorf("agent scorer not implemented yet")
	}
	out, err := step{sc.Score, filepath.Join(opts.ConfigDir, sc.Dir)}.output(opts.ConfigDir, map[string]string{
		"PATS_RUN_ID":        filepath.Base(runDir),
		"PATS_RUN_DIR":       runDir,
		"PATS_OUTPUT_DIR":    outDir,
		"PATS_TASK_DIR":      taskDir,
		"PATS_AGENT_ID":      agent,
		"PATS_TASK_ID":       task,
		"PATS_SCORER_ID":     sc.ID,
		"PATS_SCORER_VECTOR": sc.Vector,
		"PATS_MODEL":         info.model,
		"PATS_AGENT_HARNESS": info.harness,
	})
	if err != nil { // non-zero exit = failure
		return 0, fmt.Errorf("run: %w", err)
	}
	return parseScore(string(out))
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

func aggregate(runDir string, results []ScoreResult, testPairs []config.TestPair) *ScoreReport {
	// per (agent,task): mean over scorers.
	type acc struct {
		n   int
		sum float64
	}
	pair := map[string]*acc{}
	for _, c := range results {
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
		Version: scoresVersion, RunDir: runDir, Scores: results,
		PerPair: perPair, PerAgent: perAgent, Overall: overall,
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
	skipped := 0
	for _, name := range names {
		o := opts
		o.RunDir = filepath.Join(base, name)
		o.noReport = true
		r, err := Score(cfg, o)
		switch {
		case errors.Is(err, errNothingToScore):
			// an older run may predate the agents, tasks or scorers selected.
			logw{opts.Out, useColor(opts.Out)}.info("run %s: %v, skipped", name, err)
			skipped++
		case err != nil:
			return nil, fmt.Errorf("run %s: %w", name, err)
		default:
			rep = r
		}
	}
	if skipped == len(names) {
		return nil, fmt.Errorf("%w in any run", errNothingToScore)
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
// suffix) so 20260621-10 beats 20260621-2. entries that don't parse as a
// <nnn>-<yyyymmdd>-<adj>-<noun> run name (a stray directory, say) are skipped
// -- otherwise one could sort as date "" / number 0 and get picked as
// "latest", or show up in `pats list runs`.
func sortedRunNames(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, _, ok := splitRunName(e.Name()); !ok {
			continue
		}
		names = append(names, e.Name())
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

// mergeScores folds a narrowed score's results into the run's existing
// scores.json: those in scope (what this score set out to compute) are
// replaced by what it computed, the rest are kept unless the config
// no longer has their (agent, task, scorer), and the report is aggregated
// over every suite's pairs.
func mergeScores(cfg *config.Config, runDir string, results []ScoreResult, scope map[ScoreResult]bool) (*ScoreReport, error) {
	allPairs, err := cfg.ExpandTestPairs()
	if err != nil {
		return nil, err
	}
	allScore, err := cfg.ExpandScorePairs()
	if err != nil {
		return nil, err
	}
	scorersOf := map[string][]string{}
	for _, sp := range allScore {
		scorersOf[sp.Task] = append(scorersOf[sp.Task], sp.Scorer)
	}
	valid := map[ScoreResult]bool{}
	for _, tp := range allPairs {
		for _, sc := range scorersOf[tp.Task] {
			valid[ScoreResult{Agent: tp.Agent, Task: tp.Task, Scorer: sc}] = true
		}
	}

	merged := append([]ScoreResult(nil), results...)
	prev, err := readScores(runDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, errOldScores):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w (a narrowed score merges into it; score without filters to rebuild it)", err)
	default:
		for _, r := range prev.Scores {
			key := ScoreResult{Agent: r.Agent, Task: r.Task, Scorer: r.Scorer}
			if !scope[key] && valid[key] {
				merged = append(merged, r)
			}
		}
	}
	return aggregate(runDir, merged, allPairs), nil
}
