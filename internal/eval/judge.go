package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
)

// JudgeOptions configures pats judge (docs/spec/agent-scorers.md).
type JudgeOptions struct {
	ConfigDir string
	RunDir    string // -r: one run, the latest when ""
	Runs      int    // --runs: the latest this many runs, each in turn
	Jobs      int    // judge jobs at once; 0 -> 1, negative -> auto
	Repeat    int    // samples per judgement; 0 or 1 is one
	Budget    float64
	Offline   bool
	Suites    []string
	Agents    []string
	Tasks     []string
	Scorers   []string
	Out       io.Writer
	Verbose   int
}

// judgement is one agent score of one run: the pair a scorer grades, and
// where the table put it. its samples run as judge jobs; the last to end
// commits or drops the set.
type judgement struct {
	runDir      string
	agent, task string
	sc          config.Scorer
	row         judgeRow

	mu        sync.Mutex
	pending   int
	staged    bool
	samples   []verdict
	committed *verdict // the set swapped in; nil when none was
	saveErr   error    // the set could not be saved: the old one stands
}

func (j *judgement) dir() string { return judgementDir(j.runDir, j.sc.Name, j.agent, j.task) }

func (j *judgement) key() ScoreResult {
	return ScoreResult{Agent: j.agent, Task: j.task, Scorer: j.sc.Name}
}

// Judge runs the agent scorers of the selection over one run or the latest
// few, each judgement Repeat times, and merges their scores into each run's
// scores.json.
func Judge(cfg *config.Config, jo JudgeOptions) (err error) {
	if abs, err := filepath.Abs(jo.ConfigDir); err == nil {
		jo.ConfigDir = abs
	}
	color := useColor(jo.Out)
	lg := logw{jo.Out, color, jo.Verbose}
	unlock, err := lockConfigDir(jo.ConfigDir)
	if err != nil {
		return err
	}
	defer unlock()

	base := filepath.Join(jo.ConfigDir, runsSubdir)
	var runDirs []string
	if jo.Runs > 0 {
		if runDirs, err = latestRuns(base, jo.Runs); err != nil {
			return err
		}
	} else {
		dir, err := resolveRunDir(base, jo.RunDir)
		if err != nil {
			return err
		}
		runDirs = []string{dir}
	}
	for i, d := range runDirs {
		if abs, err := filepath.Abs(d); err == nil {
			runDirs[i] = abs
		}
		if err := inRuns(base, runDirs[i]); err != nil {
			return err
		}
	}

	byRun, err := judgeSelection(cfg, jo, runDirs)
	if err != nil {
		return err
	}
	var toJudge []*judgement
	total := 0
	for _, js := range byRun {
		total += len(js)
		for _, j := range js {
			if j.row == rowJudge {
				toJudge = append(toJudge, j)
			}
		}
	}
	if total == 0 {
		return fmt.Errorf("%w: no pair with output has a selected agent scorer", errNothingToScore)
	}
	// every run's scores.json is merged into: one of another format would
	// stop the merge after the money is spent, so it stops here instead.
	for _, d := range runDirs {
		if _, err := readScores(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() {
		if ctx.Err() != nil {
			err = ErrInterrupted
		}
	}()
	defer sandbox.Shutdown()

	ro := Options{ConfigDir: jo.ConfigDir, Out: jo.Out, Color: color, Verbose: jo.Verbose, Offline: jo.Offline, Budget: jo.Budget}
	n := max(jo.Repeat, 1)
	var judgePairs []config.TestPair
	seen := map[string]bool{}
	for _, j := range toJudge {
		if !seen[j.sc.Agent] {
			seen[j.sc.Agent] = true
			judgePairs = append(judgePairs, config.TestPair{Agent: j.sc.Agent})
		}
	}
	jobs := 1
	if len(toJudge) > 0 {
		var judged []config.Scorer
		for _, j := range toJudge {
			judged = append(judged, j.sc)
		}
		if err := judgeSetup(ctx, cfg, ro, judgePairs, judged, lg); err != nil {
			return err
		}
		var names []string
		for _, p := range judgePairs {
			names = append(names, p.Agent)
		}
		lg.info("judging %d scores (x %d samples) with %s", len(toJudge), n, strings.Join(names, ", "))
		jobs = resolveJobs(jo.Jobs, func() int { return autoCPUs(ctx, cfg, judgePairs) })
	}

	ro.ledger = newLedger(jo.Budget)

	for _, d := range runDirs {
		if len(runDirs) > 1 {
			lg.info("run %s", filepath.Base(d))
		}
		if err := judgeRun(ctx, cfg, ro, d, byRun[d], n, jobs); err != nil {
			return err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	if spent, notStarted := ro.ledger.totals(); notStarted > 0 {
		lg.warn("budget $%.2f reached at $%.2f: %d judge job(s) not started", jo.Budget, spent, notStarted)
		return ErrBudget
	}
	return nil
}

// inRuns refuses a run dir outside this config's .pats/runs: the lock pats
// judge holds covers this config's runs only.
func inRuns(base, dir string) error {
	b, err1 := filepath.EvalSymlinks(base)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil || filepath.Dir(d) != b {
		return fmt.Errorf("pats judge needs a run of this config (.pats/runs/), not %s", dir)
	}
	return nil
}

// judgeSelection is every agent score the selection holds in each run, each
// placed in the judge table.
func judgeSelection(cfg *config.Config, jo JudgeOptions, runDirs []string) (map[string][]*judgement, error) {
	testPairs, err := cfg.ExpandTestPairs(jo.Suites...)
	if err != nil {
		return nil, err
	}
	if len(jo.Agents) > 0 || len(jo.Tasks) > 0 {
		if testPairs, err = cfg.FilterPairs(testPairs, jo.Agents, jo.Tasks); err != nil {
			return nil, err
		}
	}
	scorePairs, err := cfg.ExpandScorePairs(jo.Suites...)
	if err != nil {
		return nil, err
	}
	if scorePairs, err = cfg.FilterScorePairs(scorePairs, jo.Scorers); err != nil {
		return nil, err
	}
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.Name })
	byTask := map[string][]config.Scorer{}
	for _, sp := range scorePairs {
		sc := scorers[sp.Scorer]
		if !sc.IsAgent() {
			if len(jo.Scorers) > 0 {
				return nil, fmt.Errorf("scorer %q is a script scorer: pats score runs it", sc.Name)
			}
			continue
		}
		byTask[sp.Task] = append(byTask[sp.Task], sc)
	}
	out := map[string][]*judgement{}
	for _, d := range runDirs {
		for _, tp := range testPairs {
			pairDir := filepath.Join(d, tp.Agent, tp.Task)
			if _, err := os.Stat(pairDir); err != nil {
				continue // the pair did not run here
			}
			for _, sc := range byTask[tp.Task] {
				out[d] = append(out[d], &judgement{runDir: d, agent: tp.Agent, task: tp.Task, sc: sc, row: judgeRowOf(pairDir)})
			}
		}
	}
	return out, nil
}

// judgeSetup does for the judge agents what Run does for its agents, with
// the same code: their pins, the model check, the credential preflight.
func judgeSetup(ctx context.Context, cfg *config.Config, ro Options, judgePairs []config.TestPair, judged []config.Scorer, lg logw) error {
	warnUnboxed(cfg, judgePairs, lg)
	// a judged scorer's own steps may run in a step sandbox, pinned with the
	// rest.
	steps := map[string]bool{}
	for _, sc := range judged {
		if sc.StepSandbox != "" {
			if name, err := cfg.One("sandbox", sc.StepSandbox); err == nil {
				steps[name] = true
			}
		}
	}
	checks, err := usePins(ctx, cfg, ro, judgePairs, steps)
	if err != nil {
		return err
	}
	upstream := make(chan []string, 1)
	if ro.Offline {
		upstream <- nil
	} else {
		warns, err := checkModels(ctx, catalogueAgents(cfg, pairAgents(cfg, judgePairs), "openrouter"), http.DefaultClient)
		if err != nil {
			return err
		}
		for _, w := range warns {
			lg.warn("%s", w)
		}
		go func() { upstream <- checkUpstream(ctx, checks, upstreamBudget) }()
	}
	lg.detail("preflight: checking judge credentials")
	if err := Preflight(ctx, cfg, ro, judgePairs, judged...); err != nil {
		return err
	}
	for _, w := range <-upstream {
		lg.warn("%s", w)
	}
	return nil
}

// judgeRun judges one run's judgements, n samples each, up to jobs at once,
// then writes its scores.json and drops what is no longer a judgement.
func judgeRun(ctx context.Context, cfg *config.Config, ro Options, runDir string, js []*judgement, n, jobs int) error {
	lg := logw{ro.Out, ro.Color, ro.Verbose}
	for _, j := range js {
		if err := repairJudgement(j.dir()); err != nil {
			return err
		}
	}
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })
	slots := newSlots(cfg, jobs)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	for _, j := range js {
		if j.row != rowJudge {
			continue
		}
		j.pending = n
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(j *judgement, i int) {
				defer wg.Done()
				defer j.sampleDone(ctx, n, lg)
				a := agents[j.sc.Agent]
				defer slots.take(cfg, a)()
				if ctx.Err() != nil {
					return
				}
				stat := &pairStat{}
				if !ro.ledger.admit(stat) {
					return
				}
				defer ro.ledger.finish(stat)
				po := ro
				var buf bytes.Buffer
				if jobs > 1 {
					po.Out = &buf
				}
				v := judgeSample(ctx, cfg, po, j, a, i, n, sandboxes, stat)
				if jobs > 1 {
					outMu.Lock()
					ro.Out.Write(buf.Bytes())
					outMu.Unlock()
				}
				j.mu.Lock()
				if v != nil {
					j.samples = append(j.samples, *v)
				}
				j.mu.Unlock()
			}(j, i)
		}
	}
	wg.Wait()

	// the scope is every agent score the selection holds and this judge
	// settled: a judgement committed, or one the table does not judge. one
	// cancelled or held back entirely keeps its old number.
	var results []ScoreResult
	scope := map[ScoreResult]bool{}
	judged, failed, na, skipped := 0, 0, 0, 0
	for _, j := range js {
		switch {
		case j.row == rowSkipped:
			skipped++
			scope[j.key()] = true
		case j.row == rowNA:
			na++
			scope[j.key()] = true
		case j.committed != nil:
			scope[j.key()] = true
			judged++
			if v := j.committed; v.OK && v.Score != nil {
				r := j.key()
				r.Score = *v.Score
				results = append(results, r)
			} else if !v.OK {
				failed++
			}
		}
	}
	rep, err := mergeScores(cfg, runDir, results, scope)
	if err != nil {
		return err
	}
	// a judged scorer's own steps ran in its step sandbox: recorded with the
	// score, as a script scorer's are.
	for _, j := range js {
		if j.sc.StepSandbox == "" || j.committed == nil || !j.committed.OK {
			continue
		}
		if name, err := cfg.One("sandbox", j.sc.StepSandbox); err == nil {
			for _, sb := range cfg.Sandboxes {
				if sb.Name == name {
					if rep.Steps == nil {
						rep.Steps = map[string]stepsRecord{}
					}
					rep.Steps[j.sc.Name] = stepsRecord{Sandbox: name, Image: sb.Image}
				}
			}
		}
	}
	if err := writeJSON(filepath.Join(runDir, "scores.json"), rep); err != nil {
		return err
	}
	// only now: a crash before the write leaves these dirs, which no reader
	// trusts for a pair the table does not judge.
	for _, j := range js {
		if j.row != rowJudge {
			for _, d := range []string{j.dir(), stagingDir(j.dir()), asideDir(j.dir())} {
				if err := os.RemoveAll(d); err != nil {
					return err
				}
			}
		}
	}
	lg.info("%s: %d judged (%d failed), %d na, %d skipped", filepath.Base(runDir), judged, failed, na, skipped)
	unsaved := 0
	for _, j := range js {
		if j.saveErr != nil {
			unsaved++
		}
	}
	if unsaved > 0 {
		return fmt.Errorf("%s: %d judgement(s) could not be saved; their earlier verdicts and scores stand", filepath.Base(runDir), unsaved)
	}
	return nil
}

// sampleDone ends one of j's samples; the last one ends the judgement: its
// set swaps in whole, or, when cancelled, its staging goes and the old set
// stays.
func (j *judgement) sampleDone(ctx context.Context, n int, lg logw) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pending--
	if j.pending > 0 || !j.staged {
		return // the budget held back every sample: no staging was made
	}
	staging := stagingDir(j.dir())
	if ctx.Err() != nil {
		os.RemoveAll(staging)
		return
	}
	v := combine(n, j.samples, verdict{
		FooterVersion: footerVersion, Judge: j.sc.Agent, Scorer: j.sc.Name,
		Judged: judged{Agent: j.agent, Task: j.task},
	})
	err := writeJSON(filepath.Join(staging, "verdict.json"), v)
	if err == nil {
		err = swapIn(j.dir())
	}
	if err != nil {
		os.RemoveAll(staging)
		j.saveErr = err
		lg.error("%s %s/%s: %v (its earlier verdict and score stand)", j.sc.Name, j.agent, j.task, err)
		return
	}
	j.committed = &v
}

// stagingDir is where a judgement at dir stages its new set, and asideDir
// where the old set waits during the swap: dot-prefixed, so no task name can
// be either.
func stagingDir(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".new")
}

func asideDir(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".old")
}

// swapIn replaces the set at dir with the one staged beside it. it fails
// whole: the old set is back at dir unless the new one is in.
func swapIn(dir string) error {
	old := asideDir(dir)
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	moved := false
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			return err
		}
		moved = true
	}
	if err := os.Rename(stagingDir(dir), dir); err != nil {
		if moved {
			if rerr := os.Rename(old, dir); rerr != nil {
				return fmt.Errorf("%w; and putting the old set back: %v", err, rerr)
			}
		}
		return err
	}
	// the new set is in: a leftover aside dir is only clutter, and the next
	// judge's repair removes it.
	os.RemoveAll(old)
	return nil
}

// repairJudgement undoes what a crash mid-judgement left at dir: a staging
// dir goes, and a set renamed aside comes back unless its successor is in.
func repairJudgement(dir string) error {
	if err := os.RemoveAll(stagingDir(dir)); err != nil {
		return err
	}
	old := asideDir(dir)
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	if _, err := os.Stat(dir); err == nil {
		return os.RemoveAll(old)
	}
	return os.Rename(old, dir)
}

// judgeSample runs sample i of j as a pair of its own, in j's staging dir,
// and writes its verdict.json. nil when the run was interrupted before it
// could say anything.
func judgeSample(ctx context.Context, cfg *config.Config, ro Options, j *judgement, a config.Agent, i, n int,
	sandboxes map[string]config.Sandbox, stat *pairStat,
) *verdict {
	lg := logw{ro.Out, ro.Color, ro.Verbose}
	staging := stagingDir(j.dir())
	j.mu.Lock()
	var stageErr error
	if !j.staged {
		if stageErr = os.RemoveAll(staging); stageErr == nil {
			stageErr = os.MkdirAll(staging, 0o755)
		}
		j.staged = true
	}
	j.mu.Unlock()

	pairDir := filepath.Join(j.runDir, j.agent, j.task)
	var info agentInfo
	var taskDir string
	for _, x := range cfg.Agents {
		if x.Name == j.agent {
			info = agentInfo{model: x.Model, harness: x.Harness}
		}
	}
	for _, t := range cfg.Tasks {
		if t.Name == j.task {
			taskDir = filepath.Join(ro.ConfigDir, t.Dir)
		}
	}
	label := fmt.Sprintf("%s %s/%s", j.sc.Name, j.agent, j.task)
	if n > 1 {
		label += " #" + strconv.Itoa(i)
	}
	jd := judged{Agent: j.agent, Task: j.task}
	job := pairJob{
		agentName: j.sc.Agent, agent: a, task: j.sc.Task(),
		outDir:    filepath.Join(staging, strconv.Itoa(i)),
		label:     label,
		names:     map[string]string{"PATS_SCORER_NAME": j.sc.Name, "PATS_SCORER_VECTOR": j.sc.Vector},
		hostNames: scoredEnv(pairDir, taskDir, j.agent, j.task, info),
		seed: func(workDir string) error {
			skipped, err := copyJudged(j.runDir, pairDir, workDir)
			if skipped > 0 {
				lg.warn("%s: left %d link(s) or special file(s) out of the judge's copy", label, skipped)
			}
			return err
		},
		footer:   judgeFooter,
		agentEnv: map[string]string{"PATS_SCORER_NAME": j.sc.Name},
		meta: func(m *pairMeta) {
			m.Judged, m.Sample = &jd, i
		},
		// in a step box the scored pair's output sits under the masked
		// runs dir: it goes back in, read-only.
		stepRO: []string{pairDir},
	}
	v := verdict{FooterVersion: footerVersion, Judge: j.sc.Agent, Scorer: j.sc.Name, Judged: jd, Sample: i}
	if stageErr != nil {
		v.Error = stageErr.Error()
		lg.error("%s %v", label, stageErr)
		return &v
	}
	err := runPair(ctx, cfg, ro, j.runDir, job, sandboxes, stat)
	if ctx.Err() != nil {
		return nil
	}
	switch {
	case err != nil:
		v.Error = err.Error()
		lg.error("%s %v", label, err)
	default:
		v = sampleVerdict(job.outDir, cfg.HarnessOf(a), v)
	}
	switch {
	case !v.OK:
		lg.warn("%s: no score: %s", label, v.Error)
	case v.NA:
		lg.detail("%s = n/a", label)
	default:
		lg.detail("%s = %.4f", label, *v.Score)
	}
	if err := os.MkdirAll(job.outDir, 0o755); err != nil {
		lg.error("%s %v", label, err)
	} else if err := writeJSON(filepath.Join(job.outDir, "verdict.json"), v); err != nil {
		lg.error("%s %v", label, err)
	}
	return &v
}

// sampleVerdict reads a finished judge's metadata and reply: the judge must
// have ended ok, with no harness error, and its reply must parse.
func sampleVerdict(outDir string, hd *machine.Harness, v verdict) verdict {
	b, err := os.ReadFile(filepath.Join(outDir, "metadata.json"))
	var m pairMeta
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	switch {
	case err != nil:
		v.Error = fmt.Sprintf("metadata: %v", err)
		return v
	case m.Status != "ok":
		v.Error = "the judge ended " + m.Status
		if m.Error != "" {
			v.Error += ": " + m.Error
		}
		return v
	case m.Summary != nil && m.Summary.IsError:
		v.Error = "the judge's harness reported an error"
		if m.Summary.Error != "" {
			v.Error += ": " + m.Summary.Error
		}
		return v
	case m.Summary != nil && m.Summary.ParseErrors > 0:
		// the line that failed may be the one that took a score back: an
		// earlier reply is not trusted over it.
		v.Error = fmt.Sprintf("the judge's output could not be read: %d line(s) failed to parse", m.Summary.ParseErrors)
		return v
	}
	score, na, err := judgeVerdict(hd, filepath.Join(outDir, "stdout.log"))
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.OK, v.NA = true, na
	if !na {
		v.Score = &score
	}
	return v
}
