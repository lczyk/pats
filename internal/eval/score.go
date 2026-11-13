package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/lczyk/pats/internal/config"
)

// ScoreOptions configures the score phase.
type ScoreOptions struct {
	ConfigDir string   // dir holding pats.yaml -- scorer paths resolve against it
	RunDir    string   // explicit run dir, or "" for the latest under .pats/runs
	Jobs      int      // max concurrent scores; 0 -> serial (1), negative -> auto (see resolveJobs)
	Suites    []string // only expand these suites (empty -> all)
	Agents    []string // filter the scored pairs to these agents (empty -> all)
	Tasks     []string // filter the scored pairs to these tasks (empty -> all)
	Scorers   []string // only run these scorers (empty -> all)
	Runs      int      // score the latest this many runs, each on its own (0: one, by RunDir)
	Out       io.Writer
	Color     bool // colour log tags (set internally from Out's tty-ness)
	Verbose   int  // -v count: 1 adds a line per score, 2 streams the scorers' output

	live io.Writer // -vv's stream when Out is a job's buffer (see liveStream)

	// noReport skips the report + stats tables (scores.json still written) --
	// set by scoreAll, where per-run tables would just scroll past.
	noReport bool
}

// scoresVersion is scores.json's own format. a file of any other version was
// written by another pats: it reads as unscored and is never merged into.
const scoresVersion = 3

// ScoreReport is the aggregated result of scoring a run, kept as scores.json.
// it is pats's own state; `pats report --json` is the interface (report.go).
type ScoreReport struct {
	Version  int                `json:"version"`
	RunDir   string             `json:"run_dir"`
	Scores   []ScoreResult      `json:"scores"`
	PerPair  map[string]float64 `json:"per_pair"` // "agent/task" -> mean over scorers
	PerAgent map[string]float64 `json:"per_agent"`
	Overall  float64            `json:"overall"`
	// FailedPairs counts, per agent, the pairs no judge can grade (nothing
	// came home, or the harness reported an error): they drop out of the
	// means, and this shows how many did.
	FailedPairs map[string]int `json:"failed_pairs"`
	// Steps is where each scorer that names a step sandbox ran its steps.
	Steps map[string]stepsRecord `json:"steps,omitempty"`
}

// ScoreResult is one scorer's score for one pair.
type ScoreResult struct {
	Agent  string  `json:"agent"`
	Task   string  `json:"task"`
	Scorer string  `json:"scorer"`
	Score  float64 `json:"score"`
}

// Score runs each suite's tasks x script scorers over a run's collected
// outputs, takes each agent score from its stored verdict, and aggregates. RunDir "all" scores every run under .pats/runs, oldest first,
// with no report tables printed (the returned report is the last run's).
func Score(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	if opts.RunDir == "all" {
		return scoreAll(cfg, opts)
	}
	if opts.Runs > 0 {
		return scoreLatest(cfg, opts)
	}
	// absolute config dir: scorers run with cwd=ConfigDir; their file path +
	// PATS_OUTPUT_DIR must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	opts.Color = useColor(opts.Out)
	lg := logw{opts.Out, opts.Color, opts.Verbose}
	unlock, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	runDir, err := resolveRunDir(filepath.Join(opts.ConfigDir, runsSubdir), opts.RunDir)
	if err != nil {
		return nil, err
	}
	lg.detail("scoring: %s", relToCwd(runDir))

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
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.Name })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.Name })
	agents := map[string]agentInfo{}
	for _, a := range cfg.Agents {
		agents[a.Name] = agentInfo{model: a.Model, harness: a.Harness}
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
	// an agent score is pats judge's to make: here it is read from its
	// stored verdict, when that is good and the pair is still one a judge
	// would grade.
	var judgedScores []ScoreResult
	var stored []ScoreResult
	for _, tp := range testPairs {
		outDir := filepath.Join(runDir, tp.Agent, tp.Task)
		if _, err := os.Stat(outDir); err != nil {
			continue // pair didn't run / no output
		}
		for _, sp := range byTask[tp.Task] {
			sc := scorers[sp.Scorer]
			if sc.IsAgent() {
				key := ScoreResult{Agent: tp.Agent, Task: tp.Task, Scorer: sc.Name}
				judgedScores = append(judgedScores, key)
				if score, ok := storedVerdict(runDir, key); ok {
					key.Score = score
					stored = append(stored, key)
				}
				continue
			}
			jobs = append(jobs, scoreJob{tp.Agent, tp.Task, outDir, filepath.Join(opts.ConfigDir, tasks[tp.Task].Dir), sc})
		}
	}
	// a narrowed score that reaches nothing must not rewrite scores.json empty.
	narrowed := len(opts.Suites) > 0 || pairFilter || len(opts.Scorers) > 0
	if len(jobs) == 0 && len(judgedScores) == 0 && narrowed {
		return nil, fmt.Errorf("%w: no pair with output in this run has a selected scorer", errNothingToScore)
	}

	njobs := resolveJobs(opts.Jobs, runtime.NumCPU)

	// a scorer's steps run in its step sandbox when it names one: pinned as
	// a run's are, then a pool of boxes per sandbox (scorePool).
	places := map[string]*stepPlace{}
	steps := map[string]bool{}
	for _, j := range jobs {
		if j.sc.StepSandbox == "" {
			continue
		}
		if name, err := cfg.One("sandbox", j.sc.StepSandbox); err == nil {
			steps[name] = true
		}
	}
	if len(steps) > 0 {
		if _, err := usePins(context.Background(), cfg, Options{ConfigDir: opts.ConfigDir, Out: opts.Out, Color: opts.Color, Verbose: opts.Verbose}, nil, steps); err != nil {
			return nil, err
		}
		sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })
		for _, j := range jobs {
			if _, ok := places[j.sc.Name]; ok || j.sc.StepSandbox == "" {
				continue
			}
			sp, err := stepPlaceOf(cfg, sandboxes, j.sc.StepSandbox, j.sc.StepEnv)
			if err != nil {
				return nil, fmt.Errorf("scorer %q: %w", j.sc.Name, err)
			}
			places[j.sc.Name] = sp
		}
	}
	pool := &scorePool{opts: opts, runDir: runDir, lg: lg, idle: map[string][]*stepBox{}}
	defer pool.close()

	var bar *progress
	if isProgressTTY(opts.Out) {
		labelW := 0
		for _, j := range jobs {
			if w := len(j.agent) + len("/") + len(j.task) + len("/") + len(j.sc.Name); w > labelW {
				labelW = w
			}
		}
		bar = newProgress(opts.Out, len(jobs), labelW)
		defer bar.close()
	} else if njobs > 1 {
		lg.detail("computing %d scores, up to %d in parallel", len(jobs), njobs)
	}

	// same bounded-worker-pool pattern as Run: buffered per-job output emitted
	// atomically on completion so concurrent progress/log writes don't interleave.
	// a scorer runs in a process group of its own, out of the terminal's
	// reach: ctrl-c reaches it through ctx.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sem := make(chan struct{}, njobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := append([]ScoreResult(nil), stored...)
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

			label := j.agent + "/" + j.task + " " + j.sc.Name
			po := opts
			var buf bytes.Buffer
			if bar != nil || njobs > 1 {
				po.Out = &buf
				if opts.Verbose >= 2 {
					ls := liveStream(label, bar, opts.Out, &mu)
					defer ls.flush()
					po.live = ls
				}
			}
			jlg := logw{po.Out, opts.Color, opts.Verbose}
			if bar != nil {
				bar.start(label, &pairStat{}, "", false)
			}

			score, serr := runScorer(ctx, po, j.sc, runDir, j.outDir, j.taskDir, j.agent, j.task, agents[j.agent], places[j.sc.Name], pool)
			switch {
			case errors.Is(serr, errScorerNA):
				jlg.detail("%s/%s %s = n/a", j.agent, j.task, j.sc.Name)
			case serr != nil:
				jlg.error("%s/%s %s: %v", j.agent, j.task, j.sc.Name, serr)
			default:
				jlg.detail("%s/%s %s = %.4f", j.agent, j.task, j.sc.Name, score)
				mu.Lock()
				results = append(results, ScoreResult{j.agent, j.task, j.sc.Name, score})
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
	// an interrupted score writes nothing: its results are a part, and the
	// unnarrowed path would replace the file with them.
	if ctx.Err() != nil {
		return nil, ErrInterrupted
	}

	var rep *ScoreReport
	if narrowed {
		scope := map[ScoreResult]bool{}
		for _, j := range jobs {
			scope[ScoreResult{Agent: j.agent, Task: j.task, Scorer: j.sc.Name}] = true
		}
		for _, k := range judgedScores {
			scope[k] = true
		}
		if rep, err = mergeScores(cfg, runDir, results, scope); err != nil {
			return nil, err
		}
	} else {
		rep = aggregate(runDir, results, testPairs)
	}
	// a stored judge score keeps where its scorer's steps ran.
	if prev, err := readScores(runDir); err == nil {
		for _, r := range stored {
			if rec, ok := prev.Steps[r.Scorer]; ok {
				if rep.Steps == nil {
					rep.Steps = map[string]stepsRecord{}
				}
				rep.Steps[r.Scorer] = rec
			}
		}
	}
	for name, sp := range places {
		if rep.Steps == nil {
			rep.Steps = map[string]stepsRecord{}
		}
		rep.Steps[name] = stepsRecord{Sandbox: sp.sb.Name, Image: sp.sb.Image}
	}
	if err := pool.close(); err != nil {
		lg.warn("scorer step sandboxes: %v", err)
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
// name in Score's agents map -- one small struct beats a second parallel map
// alongside runScorer's already-long argument list.
type agentInfo struct {
	model   string
	harness string
}

// runScorer runs one scorer over an output dir and returns its [0,1] score.
// script scorers run on the host (trusted user scripts) in the dir they were
// defined in. agent scorers pending (they will run under ctx).
func runScorer(ctx context.Context, opts ScoreOptions, sc config.Scorer, runDir, outDir, taskDir, agent, task string, info agentInfo, sp *stepPlace, pool *scorePool) (float64, error) {
	if sc.IsAgent() {
		return 0, fmt.Errorf("agent scorer %q is pats judge's to run", sc.Name)
	}
	var tee io.Writer
	switch {
	case opts.Verbose < 2:
	case opts.live != nil:
		tee = opts.live
	default:
		ls := &lineStream{emit: func(s string) { fmt.Fprintln(opts.Out, s) }}
		defer ls.flush()
		tee = ls
	}
	env := scoredEnv(outDir, taskDir, agent, task, info)
	maps.Copy(env, map[string]string{
		"PATS_RUN_NAME":      filepath.Base(runDir),
		"PATS_RUN_DIR":       runDir,
		"PATS_SCORER_NAME":   sc.Name,
		"PATS_SCORER_VECTOR": sc.Vector,
	})
	s := step{sc.Score, filepath.Join(opts.ConfigDir, sc.Dir)}
	var out []byte
	var err error
	if sp != nil {
		var box *stepBox
		if box, err = pool.get(ctx, sp); err != nil {
			return 0, err
		}
		out, err = box.output(ctx, sp, s, opts.ConfigDir, env, tee)
		pool.put(sp, box)
	} else {
		out, err = s.outputTee(ctx, opts.ConfigDir, env, tee)
	}
	if err != nil { // non-zero exit = failure
		return 0, fmt.Errorf("run: %w", err)
	}
	return parseScore(string(out))
}

// scoredEnv names the pair a scorer grades, for every scorer, script or
// agent. the unprefixed names (PATS_OUTPUT_DIR, PATS_AGENT_NAME, ...) are
// left to the run of the agent a step belongs to: a judge's, not the
// graded one's.
func scoredEnv(outDir, taskDir, agent, task string, info agentInfo) map[string]string {
	return map[string]string{
		"PATS_SCORED_DIR":        outDir,
		"PATS_SCORED_AGENT_NAME": agent,
		"PATS_SCORED_TASK_NAME":  task,
		"PATS_SCORED_TASK_DIR":   taskDir,
		"PATS_SCORED_MODEL":      info.model,
		"PATS_SCORED_HARNESS":    info.harness,
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
	for name, a := range agent {
		if a.n > 0 {
			perAgent[name] = a.sum / float64(a.n)
			osum += perAgent[name]
		}
	}
	overall := 0.0
	if len(perAgent) > 0 {
		overall = osum / float64(len(perAgent))
	}
	return &ScoreReport{
		Version: scoresVersion, RunDir: runDir, Scores: results,
		PerPair: perPair, PerAgent: perAgent, Overall: overall,
		FailedPairs: failedPairs(runDir, testPairs),
	}
}

// failedPairs counts, per agent, the pairs of testPairs in runDir that the
// judge table leaves unjudged (its na rows). a pair with no metadata is not
// counted: its status is unknown.
func failedPairs(runDir string, testPairs []config.TestPair) map[string]int {
	out := map[string]int{}
	for _, tp := range testPairs {
		if judgeRowOf(filepath.Join(runDir, tp.Agent, tp.Task)) == rowNA {
			out[tp.Agent]++
		}
	}
	return out
}

// storedVerdict is an agent score's number from its stored verdict.json:
// ok only when the verdict is good and numeric, and the pair is one the
// judge table would grade.
func storedVerdict(runDir string, k ScoreResult) (float64, bool) {
	if judgeRowOf(filepath.Join(runDir, k.Agent, k.Task)) != rowJudge {
		return 0, false
	}
	v, err := readVerdict(judgementDir(runDir, k.Scorer, k.Agent, k.Task))
	if err != nil || !v.OK || v.Score == nil {
		return 0, false
	}
	return *v.Score, true
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
	return scoreRuns(cfg, opts, base, names)
}

// scoreLatest scores the latest opts.Runs runs, as scoreAll does every one.
func scoreLatest(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	configDir, err := filepath.Abs(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(configDir, runsSubdir)
	dirs, err := latestRuns(base, opts.Runs)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirs {
		names = append(names, filepath.Base(d))
	}
	o := opts
	o.Runs = 0
	return scoreRuns(cfg, o, base, names)
}

// scoreRuns scores the named runs under base in turn: one with nothing to
// score under the filters is skipped, any other error stops.
func scoreRuns(cfg *config.Config, opts ScoreOptions, base string, names []string) (*ScoreReport, error) {
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
			logw{opts.Out, useColor(opts.Out), 0}.info("run %s: %v, skipped", name, err)
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
	var prevSteps map[string]stepsRecord
	var kept []ScoreResult
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
				kept = append(kept, r)
			}
		}
		prevSteps = prev.Steps
	}
	rep := aggregate(runDir, merged, allPairs)
	// where a kept score's scorer ran its steps stays with it; a scorer this
	// invocation ran is recorded as it ran now, by the caller.
	for _, r := range kept {
		if rec, ok := prevSteps[r.Scorer]; ok {
			if rep.Steps == nil {
				rep.Steps = map[string]stepsRecord{}
			}
			rep.Steps[r.Scorer] = rec
		}
	}
	return rep, nil
}

// scorePool lends a run's scorers step boxes: per step sandbox, a box idle
// or a new one, so at most -j per sandbox, one scorer at a time in each. a
// box binds the tree and the run dir read-only. close ends them all and
// gathers each sandbox's audits into .steps/ in the run dir.
type scorePool struct {
	opts   ScoreOptions
	runDir string
	lg     logw
	mu     sync.Mutex
	idle   map[string][]*stepBox
	all    []*stepBox
	closed bool
}

func (p *scorePool) get(ctx context.Context, sp *stepPlace) (*stepBox, error) {
	p.mu.Lock()
	if bs := p.idle[sp.sb.Name]; len(bs) > 0 {
		b := bs[len(bs)-1]
		p.idle[sp.sb.Name] = bs[:len(bs)-1]
		p.mu.Unlock()
		return b, nil
	}
	p.mu.Unlock()
	mounts, err := stepBindsFor(p.opts.ConfigDir, nil, []string{p.runDir})
	if err != nil {
		return nil, err
	}
	b, err := openStepBox(ctx, sp, mounts, "score-egress.log", configProbe(p.opts.ConfigDir))
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.all = append(p.all, b)
	p.mu.Unlock()
	return b, nil
}

// put takes a box back; one whose last exec failed to run goes, and the
// next scorer gets a fresh one.
func (p *scorePool) put(sp *stepPlace, b *stepBox) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.broken {
		_ = b.sess.Close()
		return
	}
	p.idle[sp.sb.Name] = append(p.idle[sp.sb.Name], b)
}

func (p *scorePool) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	audits := map[string][]byte{}
	var errs []error
	for _, b := range p.all {
		_ = b.sess.Close()
		if a, err := os.ReadFile(filepath.Join(b.staging, "score-egress.log")); err == nil {
			audits[b.sb.Name] = append(audits[b.sb.Name], a...)
		}
		_ = b.close("")
	}
	if len(audits) > 0 {
		dir := filepath.Join(p.runDir, ".steps")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for name, a := range audits {
			f := filepath.Join(dir, "score-egress-"+name+".log")
			if err := os.WriteFile(f, a, 0o644); err != nil {
				errs = append(errs, err)
				continue
			}
			a := readEgressAudit(f)
			if denied := append(a.Denied, a.Blocked...); len(denied) > 0 {
				p.lg.warn("scorers' step sandbox %s egress denied %d host(s) (see %s): %s", name, len(denied), relToCwd(f), strings.Join(denied, ", "))
			}
		}
	}
	return errors.Join(errs...)
}
