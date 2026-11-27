package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// pairJob is one agent call: an agent on a task, a suite's own or one an
// agent scorer synthesises, and where its files go.
type pairJob struct {
	agentName string
	agent     config.Agent
	task      config.Task
	outDir    string
	label     string // the job in log lines: agent/task
	// names is what names the task to its steps, host and in-sandbox: a
	// task's PATS_TASK_*, or a judge's PATS_SCORER_*.
	names map[string]string
	// hostNames go to the host steps only (a judge's PATS_SCORED_*: host paths,
	// and nothing the sandbox may know).
	hostNames map[string]string
	// seed fills the fresh workdir before stage; nil leaves it empty.
	seed func(workDir string) error
	// footer goes after the resolved prompt.
	footer string
	// agentEnv goes into the agent's own env in the sandbox, over what the
	// harness sets.
	agentEnv map[string]string
	// meta adds the job's own fields to its metadata.json.
	meta func(*pairMeta)
	// stepRO are host dirs a step box binds read-only beside the pair's
	// own (a judge's scored pair).
	stepRO []string
	// gate holds the pair between its setup and its agent; nil for none.
	gate *preflightGate
}

// taskJob is pair p of a run: its agent on its task, into the pair dir.
func taskJob(runDir string, p config.TestPair, a config.Agent, t config.Task) pairJob {
	return pairJob{
		agentName: p.Agent, agent: a, task: t,
		outDir: filepath.Join(runDir, p.Agent, p.Task),
		label:  p.Agent + "/" + p.Task,
		names:  map[string]string{"PATS_TASK_NAME": p.Task, "PATS_TASK_VECTOR": t.Vector},
	}
}

// runPair prepares, runs, and collects one agent call. stat must be
// non-nil (every caller passes a fresh &pairStat{}) -- it's dereferenced
// unconditionally below, both to tap live counters and to record the final
// net count.
func runPair(
	ctx context.Context, cfg *config.Config, opts Options, runDir string, job pairJob,
	sandboxes map[string]config.Sandbox, stat *pairStat,
) error {
	a, t, outDir := job.agent, job.task, job.outDir
	var tm pairTimings
	began := time.Now()
	stat.timings = func() *pairTimings {
		tm.TotalS = time.Since(began).Seconds()
		return tm.rounded()
	}
	stat.price = opts.prices[a.Name]
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return local(err)
	}
	workDir, err := sandbox.MkTemp("pats-work-")
	if err != nil {
		return err
	}
	defer sandbox.RemoveTemp(workDir) // safe: agent runs as host uid, files are host-owned
	if job.seed != nil {
		if err := job.seed(workDir); err != nil {
			return fmt.Errorf("seed workdir: %w", err)
		}
	}

	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return err
	}
	sb := sandboxes[sbID]

	// host-side env for the task's scripts (stage, prompt-cmd, collect). they
	// run in the task's directory and seed/gather the sandbox via these paths;
	// the task's own env goes on top (Validate keeps it off PATS_*).
	taskDir := filepath.Join(opts.ConfigDir, t.Dir)
	hostEnv := map[string]string{
		"PATS_RUN_NAME":      filepath.Base(runDir),
		"PATS_RUN_DIR":       runDir,
		"PATS_AGENT_NAME":    job.agentName,
		"PATS_AGENT_HARNESS": a.Harness,
		"PATS_MODEL":         a.Model,
		"PATS_WORKDIR":       workDir,
		"PATS_OUTPUT_DIR":    outDir,
	}
	if _, ok := job.names["PATS_TASK_NAME"]; ok {
		hostEnv["PATS_TASK_DIR"] = taskDir
	}
	maps.Copy(hostEnv, job.names)
	maps.Copy(hostEnv, job.hostNames)

	// every agent is a harness: give it a writable HOME + wire creds. this
	// precedes stage so stage can seed harness config into it.
	hd := cfg.HarnessOf(a)
	h, err := newHarness(opts, a, hd, sb, t.Egress, workDir, filepath.Base(runDir), filepath.Join(outDir, "egress.log"), job.label)
	if err != nil {
		return err
	}
	defer h.cleanup()
	if t.Sudo != nil {
		h.spec.Sudo = *t.Sudo
	}
	if len(job.agentEnv) > 0 {
		if h.spec.Env == nil {
			h.spec.Env = map[string]string{}
		}
		maps.Copy(h.spec.Env, job.agentEnv)
	}
	hostEnv["PATS_AGENT_HOME"] = h.homeDir
	for k, v := range t.Env {
		hostEnv[k] = v
	}

	// a key proxy's keys stay out of the host steps' inherited env too.
	var drop []string
	if sb.Egress.KeyProxy {
		drop = keyNames(cfg)
	}

	// where the host steps run: nil on the host, else a step sandbox,
	// started for each phase that has one (docs/spec/step-sandbox.md).
	sp, err := stepPlaceOf(cfg, sandboxes, t.StepSandbox, t.StepEnv)
	if err != nil {
		return err
	}

	// a failed setup step (stage, start, prepare) is the pair's error: the
	// agent never runs, nothing is collected, and the metadata names the step.
	var setupErr error
	var promptData []byte
	staged := func() {}
	if t.Stage != "" || t.PromptCmd != "" {
		staged = lap(&tm.StageS)
	}
	if sp != nil && (t.Stage != "" || t.PromptCmd != "") {
		promptData, setupErr = beforeBox(ctx, opts, sp, job, t, taskDir, []string{workDir, outDir, h.homeDir}, hostEnv)
	} else if stage := (step{script: t.Stage, dir: taskDir, drop: drop}); stage.set() {
		if err := runLogged(filepath.Join(outDir, "stage.log"), opts.stream(), func(w io.Writer) error {
			return stage.run(ctx, opts.ConfigDir, hostEnv, w)
		}); err != nil {
			setupErr = fmt.Errorf("stage: %w (see stage.log)", err)
		}
	}

	if setupErr == nil {
		// a prompt-cmd that ran in the box has given the prompt, empty or not.
		if sp == nil || t.PromptCmd == "" {
			if promptData, err = taskPrompt(ctx, opts.ConfigDir, taskDir, t, hostEnv, drop); err != nil {
				staged()
				return fmt.Errorf("prompt: %w", err)
			}
		}
		promptData = append(promptData, job.footer...)
		if err := os.WriteFile(h.promptPath(), promptData, 0o644); err != nil {
			return err
		}
	}
	staged()

	stdoutF, err := os.Create(filepath.Join(outDir, "stdout.log"))
	if err != nil {
		return local(err)
	}
	defer stdoutF.Close()
	stderrF, err := os.Create(filepath.Join(outDir, "stderr.log"))
	if err != nil {
		return local(err)
	}
	defer stderrF.Close()

	// agent output goes to the log file, always also tapped through statTap for
	// the in-run counters (out lines + per-harness tool parse) -- the full
	// stdout.log is still written intact. stat is never nil (see doc comment).
	stdoutW := io.MultiWriter(stdoutF, &statTap{stat: stat, scan: statScanner(hd)})

	// setup -- start, the in-sandbox prepare -- has its own budget; the
	// task's timeout covers the agent only.
	var r harnessRun
	var stopped bool      // the agent's budget or the deadline stopped it
	var budgetKind string // the cap of its agent's budget that stopped it
	var budgetAt usage    // what the pair had reported when it did
	var sess sandbox.Session
	var nested *sandbox.NestedResult
	// synced: the host copy of the agent's files is current, so collect
	// can read it.
	synced := false
	var syncErr error
	if setupErr == nil {
		sctx, scancel := context.WithTimeout(ctx, setupTimeout)
		var err error
		started := lap(&tm.StartS)
		h.spec.Progress = func(m string) { logw{opts.Out, opts.Color, opts.Verbose}.detail("%s %s", job.label, m) }
		sess, err = h.box.Start(sctx, h.spec)
		started()
		switch {
		case err != nil:
			// a file of the pair dir the start could not write (the proxy's
			// audit) is the run dir's own failure, not the sandbox's.
			if pe := (*fs.PathError)(nil); errors.As(err, &pe) && strings.HasPrefix(pe.Path, outDir+string(filepath.Separator)) {
				err = local(err)
			}
			setupErr = fmt.Errorf("start: %w", err)
			if ne := (*sandbox.NestedError)(nil); errors.As(err, &ne) {
				nested = ne.Result
			}
		default:
			defer sess.Close()
			if ns, ok := sess.(sandbox.NestedSession); ok {
				nested = ns.Nested()
			}
			if t.Prepare != "" {
				env := prepareEnv(job, filepath.Base(runDir), h.paths)
				prepared := lap(&tm.PrepareS)
				if err := runLogged(filepath.Join(outDir, "prepare.log"), opts.stream(), func(w io.Writer) error {
					return runPrepare(sctx, sess, sb.Driver, opts.ConfigDir, taskDir, t, env, h.paths.Prepare, w)
				}); err != nil {
					setupErr = fmt.Errorf("prepare: %w", err)
				}
				prepared()
			}
			// root is done: the agent gets what it left, and its sudo.
			if setupErr == nil {
				handed := lap(&tm.StartS)
				if err := sess.HandOver(sctx, h.paths.Work, h.paths.Home); err != nil {
					setupErr = fmt.Errorf("start: %w", err)
				}
				handed()
			}
		}
		scancel()
	}
	// the setup, ok or not, waits at the gate: a pair of an agent whose
	// preflight failed leaves nothing. neither the setup's timeout nor the
	// task's covers the wait.
	if job.gate != nil && !job.gate.open(job.agentName) {
		stat.waiting.Store(true)
		if opts.Verbose >= 2 {
			logw{opts.Out, opts.Color, opts.Verbose}.detail("%s waiting for the preflight", job.label)
		}
		waited := lap(&tm.GateS)
		ok := job.gate.pass(ctx, job.agentName)
		waited()
		stat.waiting.Store(false)
		if !ok {
			return discarded(setupErr)
		}
	} else if job.gate != nil && !job.gate.passes(job.agentName) {
		return discarded(setupErr)
	}
	if setupErr != nil {
		r = harnessRun{code: -1, err: setupErr}
	} else {
		var stderrW io.Writer = stderrF
		if s := opts.stream(); s != nil {
			stderrW = io.MultiWriter(stderrF, s)
		}
		// the agent gets a context of its own: its agent's budget cancels
		// it alone, and sync and collect still run on the pair's.
		actx, acancel := context.WithCancelCause(ctx)
		stat.use = func(d usage) {
			opts.ledger.add(stat, d)
			if u := stat.used(); passed(a.Budget, u) != "" {
				acancel(budgetStop{passed(a.Budget, u), u})
			}
		}
		// an agent's time below the task's timeout is a deadline of its own.
		var timer *time.Timer
		if bt, tt := a.Budget.Time, t.TimeoutDuration(); bt > 0 && (tt == 0 || bt < tt) {
			timer = time.AfterFunc(bt, func() { acancel(budgetStop{kind: "time"}) })
		}
		r = h.exec(actx, sess, stdoutW, stderrW, t.TimeoutDuration())
		if timer != nil {
			timer.Stop()
		}
		// the first cause wins: a deadline first is a timeout, a cap first
		// a budget stop; an interrupt is neither, and nor is an agent that
		// never launched.
		var bs budgetStop
		if !r.timedOut && ctx.Err() == nil && !errors.Is(r.err, sandbox.ErrLaunch) && errors.As(context.Cause(actx), &bs) {
			budgetKind, budgetAt = bs.kind, bs.at
		}
		acancel(nil)
		stopped = r.timedOut || budgetKind != ""
		// bring the agent's writes home for collect (a no-op where the
		// workdir is bound). a timed-out or stopped agent left files too; a
		// run interrupted by ctrl+c is not waited on.
		if (r.err == nil || stopped) && ctx.Err() == nil {
			sctx, scancel := context.WithTimeout(ctx, syncTimeout)
			home := lap(&tm.SyncS)
			if syncErr = sess.Sync(sctx); syncErr == nil {
				synced = true
			} else if r.err == nil {
				r.err = fmt.Errorf("sync: %w", syncErr)
			}
			home()
			scancel()
		}
	}
	status, errStr := "ok", ""
	switch {
	case ctx.Err() != nil:
		// an interrupt of the run wins over whatever else ended the agent.
		status, errStr = "error", "interrupted"
	case budgetKind != "":
		status, errStr = "budget", stopMessage(budgetKind, a.Budget, budgetAt, time.Duration(r.dur*float64(time.Second)))
		if syncErr != nil {
			errStr += fmt.Sprintf("; sync: %v", syncErr)
		}
	case r.timedOut:
		status, errStr = "timeout", fmt.Sprintf("timed out after %s", t.TimeoutDuration())
		if syncErr != nil {
			errStr += fmt.Sprintf("; sync: %v", syncErr)
		}
	case r.err != nil && errors.Is(r.err, sandbox.ErrPlace):
		// the place failed, not what ran in it (docs/spec/lxd-retry.md).
		status, errStr = "infra", r.err.Error()
	case r.err != nil:
		status, errStr = "error", r.err.Error()
	case r.code != 0:
		status = "nonzero"
	}

	lg := logw{opts.Out, opts.Color, opts.Verbose}

	// closing removes the proxy, and its log follower drains the audit's
	// tail (denied hosts included) only then: before collect, so egress.log
	// is whole before anything reads it or a step box holds the output dir.
	// Close is idempotent; the deferred one is a no-op after this.
	if sess != nil {
		closed := lap(&tm.CloseS)
		if err := sess.Close(); err != nil {
			lg.warn("%s sandbox cleanup: %v; pats clean removes what is left", job.label, err)
		}
		closed()
	}

	// collect when the agent ran, left something to gather (it finished, or
	// timed out mid-run) and its files are on the host. a launch failure left
	// nothing; a failed or skipped sync (ctrl+c) left a stale host copy.
	if collect := (step{script: t.Collect, dir: taskDir, drop: drop}); (r.err == nil || stopped) && synced && collect.set() {
		var err error
		collected := lap(&tm.CollectS)
		if sp != nil {
			err = afterBox(ctx, opts, sp, job, collect, []string{workDir, outDir}, hostEnv)
		} else {
			err = runLogged(filepath.Join(outDir, "collect.log"), opts.stream(), func(w io.Writer) error {
				return collect.run(ctx, opts.ConfigDir, hostEnv, w)
			})
		}
		collected()
		if err != nil {
			lg.warn("%s collect: %v", job.label, err)
		}
	}
	if ctx.Err() != nil {
		status, errStr = "error", "interrupted" // an interrupt during sync or collect too
	}
	// only what no rule anticipated is news; the policy's own refusals are
	// in metadata.json.
	ea := readEgressAudit(filepath.Join(outDir, "egress.log"))
	if len(ea.Denied) > 0 {
		lg.warn("%s egress denied (no rule): %s", job.label, strings.Join(ea.Denied, ", "))
	}
	if len(ea.KeyMisuse) > 0 {
		lg.warn("%s key misuse: a placeholder was refused off its binding at %s", job.label, strings.Join(ea.KeyMisuse, ", "))
	}
	if sp != nil {
		var denied []string
		for _, f := range []string{"stage-egress.log", "collect-egress.log"} {
			a := readEgressAudit(filepath.Join(outDir, f))
			denied = append(append(denied, a.Denied...), a.Blocked...)
		}
		if len(denied) > 0 {
			lg.warn("%s steps' egress denied %d host(s) (see stage-egress.log, collect-egress.log): %s", job.label, len(denied), strings.Join(denied, ", "))
		}
	}

	if status != "budget" {
		budgetKind = "" // an interrupt during sync or collect
	}
	meta := pairMeta{
		Agent: job.agentName, Harness: a.Harness, HarnessFrom: hd.From, HarnessHash: hd.Digest(),
		Model: a.Model, Effort: a.Effort, Task: t.Name,
		Sandbox: sbID, Driver: sb.Driver, SandNotBoxed: sandbox.TraitsOf(sb.Driver).Unboxed, Image: sb.Image, Sudo: h.spec.Sudo,
		ExitCode: r.code, DurationS: round2(r.dur), Status: status, Error: errStr, BudgetKind: budgetKind, Synced: &synced,
		PatsVersion:  version.Info.Version,
		DeniedEgress: ea.Denied, BlockedEgress: ea.Blocked, FailedEgress: ea.Failed, KeyMisuse: ea.KeyMisuse,
		Summary:    summarize(hd, filepath.Join(outDir, "stdout.log")),
		Components: sb.Components,
		Placed:     sb.Placed,
		Nested:     nestedRecordOf(nested),
	}
	if sb.Driver == sandbox.LXDDriver {
		meta.LXD = lxdProvenanceOf(ctx, sb)
	}
	if sp != nil {
		meta.Steps = &stepsRecord{Sandbox: sp.sb.Name, Image: sp.sb.Image}
	}
	if h.spec.Egress.Set {
		enforced := !sandbox.TraitsOf(sb.Driver).Unboxed
		meta.Egress = &egressRecord{
			Policy:   maskedPolicy(h.spec.Egress.Policy),
			Image:    proxyImageID(ctx, sb),
			Requests: ea.Requests, BytesIn: ea.In, BytesOut: ea.Out,
			Enforced:        &enforced,
			KeysSubstituted: ea.Substituted,
		}
	}
	// the ledger reads the summary's cost when the pair ends: set before
	// anything here can fail.
	meta.Summary.estimate(stat.price)
	if s := meta.Summary; s != nil {
		stat.tokens, stat.cost, stat.costKnown = s.OutputTokens, s.CostUSD, s.HasCost
		stat.toolsUsed, stat.toolsKnown = int64(s.toolCalls), true
		stat.turnsUsed, stat.turnsKnown = int64(s.Turns), true
		stat.tokensUsed = int64(s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens)
		stat.tokensKnown = s.hasTokens
		if s.ParseErrors > 0 {
			lg.warn("%s parse: %s", job.label, s.parseError)
		}
	}
	// an interrupt during the close or the lookups above still makes the
	// pair interrupted.
	if ctx.Err() != nil {
		meta.Status, meta.Error, meta.BudgetKind = "error", "interrupted", ""
	}
	if job.meta != nil {
		job.meta(&meta)
	}
	meta.Timings = stat.timings()
	if err := writeJSON(filepath.Join(outDir, "metadata.json"), meta); err != nil {
		return err
	}

	// final net count straight from the audit log (the bar's poll may lag).
	if h.spec.Egress.Set {
		atomic.StoreInt64(&stat.net, int64(ea.Requests))
		atomic.StoreInt32(&stat.netSeen, 1)
	}
	// line 1: the verdict (level by status). line 2: the stats, indented under it.
	statusLine := fmt.Sprintf("%s %s (exit %d, %.1fs)", job.label, status, r.code, r.dur)
	switch status {
	case "ok":
		lg.info("%s", statusLine)
	case "nonzero", "timeout":
		lg.warn("%s", statusLine)
	default:
		lg.error("%s", statusLine)
	}
	stat.status, stat.agentDur = status, r.dur
	stats := stat.cols(hd.Parser() != nil)
	if s := meta.Summary; s != nil {
		// cost + turns are omitted for harnesses whose stream doesn't report them.
		stats += fmt.Sprintf("  %s tok", humanK(s.OutputTokens))
		if s.HasCost {
			stats += fmt.Sprintf("  %s$%.2f", estMark(s.CostEstimated), s.CostUSD)
		}
		if s.Turns > 0 {
			stats += fmt.Sprintf("  %d turns", s.Turns)
		}
	}
	lg.detail("%s %s", job.label, stats)
	return nil
}

// pairFailed ends a pair runPair gave up on before it could say how -- its
// sandbox did not open, its prompt did not resolve, a log could not be made
// -- as an error pair: metadata.json names what failed, and the run's totals
// and its report count the pair instead of losing it.
func pairFailed(ctx context.Context, cfg *config.Config, job pairJob, sandboxes map[string]config.Sandbox, stat *pairStat, cause error) {
	a := job.agent
	synced := false
	meta := pairMeta{
		Agent: job.agentName, Harness: a.Harness, Model: a.Model, Effort: a.Effort, Task: job.task.Name,
		ExitCode: -1, Status: "error", Error: cause.Error(), Synced: &synced,
		PatsVersion: version.Info.Version,
	}
	if hd := cfg.HarnessOf(a); hd != nil {
		meta.HarnessFrom, meta.HarnessHash = hd.From, hd.Digest()
	}
	if ctx.Err() != nil {
		meta.Error = "interrupted"
	}
	if id, err := cfg.ResolveSandbox(a); err == nil {
		sb := sandboxes[id]
		meta.Sandbox, meta.Driver, meta.SandNotBoxed, meta.Image, meta.Components = id, sb.Driver, sandbox.TraitsOf(sb.Driver).Unboxed, sb.Image, sb.Components
		meta.Placed = sb.Placed
	}
	if stat.timings != nil {
		meta.Timings = stat.timings()
	}
	if job.meta != nil {
		job.meta(&meta)
	}
	stat.status = "error"
	if os.MkdirAll(job.outDir, 0o755) == nil {
		_ = writeJSON(filepath.Join(job.outDir, "metadata.json"), meta)
	}
}

// discarded is a pair dropped at the gate: its session closes as runPair
// returns, and a failed write of the run dir's own in its setup goes with
// it, for the run to report.
func discarded(setupErr error) error {
	if errors.Is(setupErr, errLocal) {
		return fmt.Errorf("%w: %w", errDiscard, setupErr)
	}
	return errDiscard
}

// runLogged runs f with its output written to logPath and, when tee is set,
// to tee too.
func runLogged(logPath string, tee io.Writer, f func(io.Writer) error) error {
	lf, err := os.Create(logPath)
	if err != nil {
		return local(err)
	}
	lw := &errWriter{w: lf}
	var w io.Writer = lw
	if tee != nil {
		w = io.MultiWriter(lw, tee)
	}
	err = f(w)
	cerr := lf.Close()
	// the log not written is the run dir's own failure, whatever the step's.
	if lw.err != nil || cerr != nil {
		return errors.Join(err, local(errors.Join(lw.err, cerr)))
	}
	return err
}

// errWriter keeps the first error of its writes, and takes no more after.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// taskPrompt is the prompt handed to the agent: the literal prompt, the
// contents of prompt-file (resolved against the task's dir), or the stdout of
// prompt-cmd, a script run after stage with the task's env.
func taskPrompt(ctx context.Context, root, dir string, t config.Task, env map[string]string, drop []string) ([]byte, error) {
	switch {
	case t.PromptFile != "":
		p := t.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return os.ReadFile(p)
	case t.PromptCmd != "":
		return step{script: t.PromptCmd, dir: dir, drop: drop}.output(ctx, root, env)
	}
	return []byte(t.Prompt), nil
}

// stderrTail renders a failed script's captured stderr for embedding in the
// error (last few lines -- enough to see the crash, not the whole log).
func stderrTail(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if s == "" {
		return ""
	}
	const keep = 10
	if lines := strings.Split(s, "\n"); len(lines) > keep {
		s = "... " + strings.Join(lines[len(lines)-keep:], "\n")
	}
	return "\nstderr: " + s
}

// beforeBox runs a pair's stage and prompt-cmd in its step sandbox, with
// the workdir, the output dir and the agent's home writable: stage's log and
// the box's audit staged and moved in once the box has synced and closed,
// prompt-cmd's stdout returned. a failure is the pair's setup error.
func beforeBox(ctx context.Context, opts Options, sp *stepPlace, job pairJob, t config.Task, taskDir string, rw []string, env map[string]string) (_ []byte, err error) {
	mounts, err := stepBindsFor(opts.ConfigDir, rw, job.stepRO)
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	box, err := openStepBox(ctx, sp, mounts, "stage-egress.log", configProbe(opts.ConfigDir))
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	defer func() {
		// the logs not moved in are the run dir's own failure, kept beside
		// the step's.
		if cerr := box.close(job.outDir); cerr != nil {
			err = errors.Join(err, local(fmt.Errorf("stage: move the step logs in: %w", cerr)))
		}
	}()
	if stage := (step{script: t.Stage, dir: taskDir}); stage.set() {
		if err := runLogged(filepath.Join(box.staging, "stage.log"), opts.stream(), func(w io.Writer) error {
			return box.run(ctx, sp, stage, opts.ConfigDir, env, w, w)
		}); err != nil {
			return nil, fmt.Errorf("stage: %w (see stage.log)", err)
		}
	}
	var prompt []byte
	if t.PromptCmd != "" {
		if prompt, err = box.output(ctx, sp, step{script: t.PromptCmd, dir: taskDir}, opts.ConfigDir, env, nil); err != nil {
			return nil, fmt.Errorf("prompt: %w", err)
		}
	}
	if err := box.sync(ctx); err != nil {
		return nil, fmt.Errorf("stage: sync: %w", err)
	}
	return prompt, nil
}

// afterBox runs a pair's collect in its step sandbox, with the workdir and
// the output dir writable, staging its log and audit as beforeBox does.
func afterBox(ctx context.Context, opts Options, sp *stepPlace, job pairJob, collect step, rw []string, env map[string]string) (err error) {
	mounts, err := stepBindsFor(opts.ConfigDir, rw, job.stepRO)
	if err != nil {
		return err
	}
	box, err := openStepBox(ctx, sp, mounts, "collect-egress.log", configProbe(opts.ConfigDir))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := box.close(job.outDir); cerr != nil && err == nil {
			err = fmt.Errorf("move the step logs in: %w", cerr)
		}
	}()
	if err := runLogged(filepath.Join(box.staging, "collect.log"), opts.stream(), func(w io.Writer) error {
		return box.run(ctx, sp, collect, opts.ConfigDir, env, w, w)
	}); err != nil {
		return err
	}
	return box.sync(ctx)
}
