// Package eval drives the run phase: expand the test-matrix and, per pair,
// prepare a sandbox, run the agent in it, and collect outputs into a run dir.
// (the score phase lands in a later phase.)
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

const runsSubdir = ".pats/runs"

// Options configures a run.
type Options struct {
	ConfigDir string    // dir holding pats.yaml -- prompt/script paths resolve against it
	Now       time.Time // wall clock, for the run-dir slug (injectable for tests)
	Out       io.Writer // progress + host script output
	Jobs      int       // max concurrent pairs; 0 -> serial (1), negative -> auto (see resolveJobs)
	Color     bool      // colour log tags (set internally from Out's tty-ness)
	Verbose   int       // -v count: 1 adds detail lines, 2 streams host scripts and the agent's stderr
	Agents    []string  // filter the pairs to these agents (empty -> all)
	Tasks     []string  // filter the pairs to these tasks (empty -> all)
	Suites    []string  // only expand these suites (empty -> all)
	Offline   bool      // skip the upstream checks, for a machine without a network
	// DryRun lists the pairs with a cost estimate and stops before the first
	// sandbox; JSON (with DryRun only) writes that list as json lines to
	// JSONOut, while Out takes the log.
	DryRun  bool
	JSON    bool
	JSONOut io.Writer
	Repeat  int     // runs of the selection, one after another; 0 or 1 is one
	MaxCost float64 // the invocation's budget in us dollars; 0 is none

	live io.Writer // -vv's stream when Out is a pair's buffer (see liveStream)
	// ledger is the invocation's spend (budget.md); nil counts nothing.
	ledger *ledger
}

// Run executes every suite (agent, task) pair and returns the run dir it wrote to.
// a single pair failing is logged and skipped -- it does not abort the run.
func Run(cfg *config.Config, opts Options) (_ string, err error) {
	// absolute config dir: stage/collect run with cwd=ConfigDir, and the
	// PATS_*_DIR paths must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	opts.Color = useColor(opts.Out) // decided once from the real terminal; po copies inherit it
	lg := logw{opts.Out, opts.Color, opts.Verbose}
	// a dry run only reads: it runs beside a live run.
	if !opts.DryRun {
		unlock, err := lockConfigDir(opts.ConfigDir)
		if err != nil {
			return "", err
		}
		defer unlock()
	}
	pairs, err := cfg.ExpandTestPairs(opts.Suites...)
	if err != nil {
		return "", err
	}
	// a config with no suites is valid to hold but runs nothing -- say that,
	// rather than letting FilterPairs blame an agent/task filter the user
	// never passed.
	if len(pairs) == 0 {
		return "", fmt.Errorf("no (agent, task) pairs to run: the selected suites expand to none (a config with no suite vector runs nothing)")
	}
	if pairs, err = cfg.FilterPairs(pairs, opts.Agents, opts.Tasks); err != nil {
		return "", err
	}
	// first output up front: the slow startup steps (image build, preflight) are
	// otherwise silent, so a bare `pats run` looks hung until the first pair logs.
	lg.detail("%d pair(s) to run", len(pairs))
	// cancel on ctrl+C so the in-flight container is torn down, not orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// ctrl-c anywhere from here (the model check, the preflight, a repeat)
	// is an interrupted run, whatever error the step it cut made of it.
	defer func() {
		if ctx.Err() != nil {
			err = ErrInterrupted // shutdown below runs first, and may be what was cut
		}
	}()
	// an lxd run's marker goes when the run ends; its lease stops.
	defer sandbox.Shutdown()

	// the images `pats init` pinned, first: everything after (preflight
	// included) starts from them, and a run never builds or pulls.
	checks, err := usePins(ctx, cfg, opts, pairs)
	if err != nil {
		return "", err
	}
	// a model openrouter does not have, or one without tools, fails here,
	// before the run dir and any sandbox.
	if !opts.Offline {
		warns, err := checkModels(ctx, pairAgents(cfg, pairs), http.DefaultClient)
		if err != nil {
			return "", err
		}
		for _, w := range warns {
			lg.warn("%s", w)
		}
	}
	if opts.DryRun {
		return "", dryRun(cfg, opts, pairs, lg)
	}
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.Name })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })

	// newer upstream images, looked up while preflight runs: a warning
	// each, never a stop.
	upstream := make(chan []string, 1)
	if opts.Offline {
		upstream <- nil
	} else {
		go func() { upstream <- checkUpstream(ctx, checks, upstreamBudget) }()
	}

	// fail fast on bad/expired creds before spinning per-pair containers.
	lg.detail("preflight: checking agent credentials")
	if err := Preflight(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}
	for _, w := range <-upstream {
		lg.warn("%s", w)
	}

	jobs := resolveJobs(opts.Jobs, func() int { return autoCPUs(ctx, cfg, pairs) })

	// the budget spans every repeat: the invocation's.
	opts.ledger = newLedger(opts.MaxCost)
	if opts.MaxCost > 0 {
		var uncapped []string
		for _, a := range pairAgents(cfg, pairs) {
			if noCostHarnesses[a.Harness] {
				uncapped = append(uncapped, a.Name)
			}
		}
		if len(uncapped) > 0 {
			lg.warn("--max-cost: %s report no cost, and run uncapped", strings.Join(uncapped, ", "))
		}
	}

	// the repeats (docs/spec/repeats.md): each an ordinary run with its own
	// run dir; what is above happens once for all of them.
	n := max(opts.Repeat, 1)
	var runDir string
	unrun, current := 0, 0 // repeats a reached budget stopped; the one it stopped in
	for i := 1; i <= n; i++ {
		if ctx.Err() != nil {
			break
		}
		if opts.ledger.reached() {
			unrun = n - i + 1
			break
		}
		current = i
		dir, err := nextRunDir(filepath.Join(opts.ConfigDir, runsSubdir), opts.Now)
		if err != nil {
			return runDir, err
		}
		runDir = dir
		if n > 1 {
			lg.info("run %d/%d: %s", i, n, filepath.Base(runDir))
		} else {
			lg.detail("run dir: %s", relToCwd(runDir))
		}
		runOnce(ctx, cfg, opts, lg, runDir, pairs, jobs, agents, tasks, sandboxes)
	}
	if ctx.Err() != nil {
		return runDir, ErrInterrupted
	}
	if spent, notStarted := opts.ledger.totals(); notStarted > 0 || unrun > 0 {
		what := fmt.Sprintf("%d pair(s)", notStarted)
		if n > 1 {
			what += fmt.Sprintf(" of run %d/%d", current, n)
		}
		if unrun > 0 {
			what += fmt.Sprintf(" and %d more run(s)", unrun)
		}
		lg.warn("budget $%.2f reached at $%.2f: %s not started", opts.MaxCost, spent, what)
		return runDir, ErrBudget
	}
	return runDir, nil
}

// ErrInterrupted is a run stopped by ctrl-c or sigterm.
var ErrInterrupted = errors.New("interrupted")

// runOnce runs every pair once into runDir, up to jobs at a time.
func runOnce(ctx context.Context, cfg *config.Config, opts Options, lg logw, runDir string, pairs []config.TestPair, jobs int,
	agents map[string]config.Agent, tasks map[string]config.Task, sandboxes map[string]config.Sandbox,
) {
	// on a tty, draw a sticky progress region (total bar + a live line per
	// in-flight pair); otherwise (pipe/ci/tee) print plain per-pair lines.
	var bar *progress
	if isProgressTTY(opts.Out) {
		labelW := 0
		for _, p := range pairs {
			if w := len(p.Agent) + len("/") + len(p.Task); w > labelW {
				labelW = w
			}
		}
		bar = newProgress(opts.Out, len(pairs), labelW)
		defer bar.close()
	} else if jobs > 1 {
		lg.detail("running %d pairs, up to %d in parallel", len(pairs), jobs)
	}

	// bounded worker pool. a failing pair is logged + skipped, never aborts
	// siblings -- so a plain semaphore (not errgroup's cancel-on-error). when
	// buffered (bar active, or jobs>1) each pair writes to its own buffer,
	// emitted atomically on completion so concurrent progress + host-script
	// output don't interleave; serial non-tty writes straight to opts.Out.
	sem := make(chan struct{}, jobs)
	// a sandbox's max-parallel caps its pairs under the run-wide -j. a pair
	// takes its sandbox's slot before a run-wide one, so pairs queued on a
	// capped sandbox never hold a slot another sandbox could use.
	sbSem := map[string]chan struct{}{}
	for _, sb := range cfg.Sandboxes {
		if sb.MaxParallel > 0 {
			sbSem[sb.Name] = make(chan struct{}, sb.MaxParallel)
		}
	}
	var wg sync.WaitGroup
	var outMu sync.Mutex
	var interrupted sync.Once
	began := time.Now()
	stats := make([]*pairStat, len(pairs))
	for i, p := range pairs {
		stats[i] = &pairStat{}
		wg.Add(1)
		go func(p config.TestPair, stat *pairStat) {
			defer wg.Done()
			if sbID, err := cfg.ResolveSandbox(agents[p.Agent]); err == nil && sbSem[sbID] != nil {
				sbSem[sbID] <- struct{}{}
				defer func() { <-sbSem[sbID] }()
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				interrupted.Do(func() {
					if bar != nil {
						bar.log(lg.line(lvWarn, "interrupted -- stopping"))
					} else {
						lg.warn("interrupted -- stopping")
					}
				})
				return
			}

			// the run-wide cap: once the spend reached it, no pair starts.
			if !opts.ledger.admit(stat) {
				return
			}
			defer opts.ledger.finish(stat)

			label := p.Agent + "/" + p.Task
			po := opts
			var buf bytes.Buffer
			if bar != nil || jobs > 1 {
				po.Out = &buf
				if opts.Verbose >= 2 {
					ls := liveStream(label, bar, opts.Out, &outMu)
					defer ls.flush()
					po.live = ls
				}
			}
			if bar != nil {
				egressPath := filepath.Join(runDir, p.Agent, p.Task, "egress.log")
				bar.start(label, stat, egressPath, statScanner(agents[p.Agent].Harness) != nil)
			}
			if err := runPair(ctx, cfg, po, runDir, taskJob(runDir, p, agents[p.Agent], tasks[p.Task]), sandboxes, stat); err != nil {
				logw{po.Out, opts.Color, opts.Verbose}.error("%s/%s %v", p.Agent, p.Task, err)
			}
			switch {
			case bar != nil:
				bar.finish(label, buf.String())
			case jobs > 1:
				outMu.Lock()
				opts.Out.Write(buf.Bytes())
				outMu.Unlock()
			}
		}(p, stats[i])
	}
	wg.Wait()
	if opts.Verbose >= 1 {
		if sum := runTotals(stats, time.Since(began)); bar != nil {
			bar.log(lg.line(lvInfo, "%s", sum))
		} else {
			lg.info("%s", sum)
		}
	}
}

// resolveJobs maps a Jobs option to a concrete worker count.
//
//	>0  -> that many
//	0   -> 1 (serial; the zero-value default)
//	<0  -> auto: autoN() (vm-accurate docker cpu count on macos for the run
//	       phase; host cpus for the score phase, which never touches docker),
//	       falling back to host cpus when autoN() reports < 1, clamped to
//	       [1,8]. pairs and scores are network-bound (model round-trips), so cpu is
//	       a rough proxy, not a real limit -- the clamp stops a big box from
//	       hammering the daemon + tripping rate-limits.
func resolveJobs(j int, autoN func() int) int {
	if j > 0 {
		return j
	}
	if j == 0 {
		return 1
	}
	n := autoN()
	if n < 1 {
		n = runtime.NumCPU()
	}
	return min(max(n, 1), 8)
}

// autoCPUs sizes -j auto by the machine the pairs run on: an lxd server's
// cpus when a sandbox the run uses is lxd, else the first used sandbox's
// daemon's.
func autoCPUs(ctx context.Context, cfg *config.Config, pairs []config.TestPair) int {
	used := usedSandboxes(cfg, pairs)
	var pick *config.Sandbox
	for i, sb := range cfg.Sandboxes {
		if used[sb.Name] && (pick == nil || (sb.Driver == "lxd" && pick.Driver != "lxd")) {
			pick = &cfg.Sandboxes[i]
		}
	}
	if pick == nil {
		return 0
	}
	h, err := hostFor(*pick)
	if err != nil {
		return 0
	}
	return h.CPUs(ctx)
}

// egressRecord is what a pair ran under and what crossed the proxy, so a run
// is reproducible from its own output.
type egressRecord struct {
	Policy   proxy.Policy `json:"policy"`
	Image    string       `json:"image,omitempty"` // the sidecar image id (docker / podman)
	Requests int          `json:"requests"`
	BytesIn  int64        `json:"bytes_in"`
	BytesOut int64        `json:"bytes_out"`
}

type pairMeta struct {
	Agent   string `json:"agent"`
	Harness string `json:"harness,omitempty"`
	// the model and effort the agent ran with: the agent id alone can be
	// pointed at another model between runs (the dry run's estimate matches
	// on them).
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Task    string `json:"task"`
	Sandbox string `json:"sandbox"`
	Image   string `json:"image"`
	Sudo    bool   `json:"sudo,omitempty"` // the agent could sudo (lxd)
	// the lxd server and image the pair ran on.
	LXD       *lxdProvenance `json:"lxd,omitempty"`
	ExitCode  int            `json:"exit_code"`
	DurationS float64        `json:"duration_s"`
	Status    string         `json:"status"` // ok | nonzero | error
	Error     string         `json:"error,omitempty"`
	// Synced is whether the agent's files came home, so collect saw them; a
	// pair from before the field has nil (see filesHome).
	Synced      *bool  `json:"synced"`
	PatsVersion string `json:"pats_version"`
	// egress the proxy refused, by kind: denied is what no rule anticipated
	// (the cheat signal), blocked is what the policy names, failed is upstream
	// trouble. see readEgressAudit.
	DeniedEgress  []string      `json:"denied_egress,omitempty"`
	BlockedEgress []string      `json:"blocked_egress,omitempty"`
	FailedEgress  []string      `json:"failed_egress,omitempty"`
	Egress        *egressRecord `json:"egress,omitempty"`
	// per-harness digest parsed from the log: cost, tokens, turns, tools.
	Summary *runSummary `json:"summary,omitempty"`
	// the harness versions inside the image, as pats init stamped them.
	Components map[string]string `json:"components,omitempty"`
	// a judge's sample: the pair it graded, and which sample it is.
	Judged *judged `json:"judged,omitempty"`
	Sample int     `json:"sample,omitempty"`
}

// filesHome says whether the pair's files came home. a pair written before
// the synced field counts as home when it ended ok or nonzero.
func (m *pairMeta) filesHome() bool {
	if m.Synced != nil {
		return *m.Synced
	}
	return m.Status == "ok" || m.Status == "nonzero"
}

// egressAudit is a pair's egress.log folded into what the run reports: the
// three refusal lists, each a unique target (the url when the proxy saw one,
// else the host), and the traffic totals.
type egressAudit struct {
	Denied   []string // rule: default -- nobody anticipated it; the cheat signal
	Blocked  []string // a policy refusal the agent triggered
	Failed   []string // upstream trouble: revoked certificate, dial failure
	Requests int      // open and req lines
	In, Out  int64    // bytes from and to upstream
}

// failedReasons are refusals the agent did not cause.
var failedReasons = map[string]bool{"revoked": true, "dial": true}

// readEgressAudit folds an audit log; a missing file is the zero value (no
// proxy ran). lines that are not json (there should be none) are skipped.
func readEgressAudit(auditPath string) egressAudit {
	var a egressAudit
	data, err := os.ReadFile(auditPath)
	if err != nil {
		return a
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e proxy.Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Ev {
		case "open", "req":
			a.Requests++
		case "close":
			a.In += e.In
			a.Out += e.Out
			continue
		default:
			continue
		}
		if e.Ev == "req" {
			a.In += e.In
			a.Out += e.Out
		}
		if e.Allowed {
			continue
		}
		target := e.Host
		if e.URL != "" {
			target = e.URL
		}
		var list *[]string
		switch {
		case failedReasons[e.Reason]:
			list = &a.Failed
		case e.Rule == "default" && e.Reason == "":
			list = &a.Denied
		default:
			list = &a.Blocked
		}
		key := fmt.Sprintf("%p\x00%s", list, target)
		if !seen[key] {
			seen[key] = true
			*list = append(*list, target)
		}
	}
	return a
}

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
	// meta adds the job's own fields to its metadata.json.
	meta func(*pairMeta)
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
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
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
	h, err := newHarness(opts, a, sb, t.Egress, workDir, filepath.Base(runDir), filepath.Join(outDir, "egress.log"), job.label)
	if err != nil {
		return err
	}
	defer h.cleanup()
	if t.Sudo != nil {
		h.spec.Sudo = *t.Sudo
	}
	hostEnv["PATS_AGENT_HOME"] = h.homeDir
	for k, v := range t.Env {
		hostEnv[k] = v
	}

	// a failed setup step (stage, start, prepare) is the pair's error: the
	// agent never runs, nothing is collected, and the metadata names the step.
	var setupErr error
	if stage := (step{t.Stage, taskDir}); stage.set() {
		if err := runLogged(filepath.Join(outDir, "stage.log"), opts.stream(), func(w io.Writer) error {
			return stage.run(ctx, opts.ConfigDir, hostEnv, w)
		}); err != nil {
			setupErr = fmt.Errorf("stage: %w (see stage.log)", err)
		}
	}

	if setupErr == nil {
		promptData, err := taskPrompt(ctx, opts.ConfigDir, taskDir, t, hostEnv)
		if err != nil {
			return fmt.Errorf("prompt: %w", err)
		}
		promptData = append(promptData, job.footer...)
		if err := os.WriteFile(h.promptPath(), promptData, 0o644); err != nil {
			return err
		}
	}

	stdoutF, err := os.Create(filepath.Join(outDir, "stdout.log"))
	if err != nil {
		return err
	}
	defer stdoutF.Close()
	stderrF, err := os.Create(filepath.Join(outDir, "stderr.log"))
	if err != nil {
		return err
	}
	defer stderrF.Close()

	// agent output goes to the log file, always also tapped through statTap for
	// the in-run counters (out lines + per-harness tool parse) -- the full
	// stdout.log is still written intact. stat is never nil (see doc comment).
	stdoutW := io.MultiWriter(stdoutF, &statTap{stat: stat, scan: statScanner(a.Harness)})

	// setup -- start, the in-sandbox prepare -- has its own budget; the
	// task's timeout covers the agent only.
	var r harnessRun
	var budget, stopped bool // the agent's max-cost stopped it; it or the deadline did
	var sess sandbox.Session
	// synced: the host copy of the agent's files is current, so collect
	// can read it.
	synced := false
	var syncErr error
	if setupErr == nil {
		sctx, scancel := context.WithTimeout(ctx, setupTimeout)
		var err error
		sess, err = h.box.Start(sctx, h.spec)
		switch {
		case err != nil:
			setupErr = fmt.Errorf("start: %w", err)
		default:
			defer sess.Close()
			if t.Prepare != "" {
				env := prepareEnv(job, filepath.Base(runDir))
				if err := runLogged(filepath.Join(outDir, "prepare.log"), opts.stream(), func(w io.Writer) error {
					return runPrepare(sctx, sess, opts.ConfigDir, taskDir, t, env, w)
				}); err != nil {
					setupErr = fmt.Errorf("prepare: %w", err)
				}
			}
			// root is done: the agent gets what it left, and its sudo.
			if setupErr == nil {
				if err := sess.HandOver(sctx, sandbox.WorkMount, homeMount); err != nil {
					setupErr = fmt.Errorf("start: %w", err)
				}
			}
		}
		scancel()
	}
	if setupErr != nil {
		r = harnessRun{code: -1, err: setupErr}
	} else {
		var stderrW io.Writer = stderrF
		if s := opts.stream(); s != nil {
			stderrW = io.MultiWriter(stderrF, s)
		}
		// the agent gets a context of its own: its agent's max-cost cancels
		// it alone, and sync and collect still run on the pair's.
		actx, acancel := context.WithCancelCause(ctx)
		stat.spend = func(c float64) {
			opts.ledger.add(stat, c)
			if spent := stat.spent(); a.MaxCost != nil && spent > *a.MaxCost {
				acancel(errBudget)
			}
		}
		r = h.exec(actx, sess, stdoutW, stderrW, t.TimeoutDuration())
		// the first cause wins: a deadline first is a timeout, a cap first
		// a budget stop; an interrupt is neither.
		budget = !r.timedOut && ctx.Err() == nil && errors.Is(context.Cause(actx), errBudget)
		acancel(nil)
		stopped = r.timedOut || budget
		// bring the agent's writes home for collect (a no-op where the
		// workdir is bound). a timed-out or stopped agent left files too; a
		// run interrupted by ctrl+c is not waited on.
		if (r.err == nil || stopped) && ctx.Err() == nil {
			sctx, scancel := context.WithTimeout(ctx, syncTimeout)
			if syncErr = sess.Sync(sctx); syncErr == nil {
				synced = true
			} else if r.err == nil {
				r.err = fmt.Errorf("sync: %w", syncErr)
			}
			scancel()
		}
	}
	status, errStr := "ok", ""
	switch {
	case ctx.Err() != nil:
		// an interrupt of the run wins over whatever else ended the agent.
		status, errStr = "error", "interrupted"
	case budget:
		status, errStr = "budget", fmt.Sprintf("cost $%.2f passed max-cost $%.2f", stat.spent(), *a.MaxCost)
		if syncErr != nil {
			errStr += fmt.Sprintf("; sync: %v", syncErr)
		}
	case r.timedOut:
		status, errStr = "timeout", fmt.Sprintf("timed out after %s", t.TimeoutDuration())
		if syncErr != nil {
			errStr += fmt.Sprintf("; sync: %v", syncErr)
		}
	case r.err != nil:
		status, errStr = "error", r.err.Error()
	case r.code != 0:
		status = "nonzero"
	}

	lg := logw{opts.Out, opts.Color, opts.Verbose}

	// collect when the agent ran, left something to gather (it finished, or
	// timed out mid-run) and its files are on the host. a launch failure left
	// nothing; a failed or skipped sync (ctrl+c) left a stale host copy.
	if collect := (step{t.Collect, taskDir}); (r.err == nil || stopped) && synced && collect.set() {
		if err := runLogged(filepath.Join(outDir, "collect.log"), opts.stream(), func(w io.Writer) error {
			return collect.run(ctx, opts.ConfigDir, hostEnv, w)
		}); err != nil {
			lg.warn("%s collect: %v", job.label, err)
		}
	}
	if ctx.Err() != nil {
		status, errStr = "error", "interrupted" // an interrupt during sync or collect too
	}

	// closing removes the proxy, and its log follower drains the audit's
	// tail (denied hosts included) only then. Close is idempotent; the
	// deferred one is a no-op after this.
	if sess != nil {
		_ = sess.Close()
	}
	// only what no rule anticipated is news; the policy's own refusals are
	// in metadata.json.
	ea := readEgressAudit(filepath.Join(outDir, "egress.log"))
	if len(ea.Denied) > 0 {
		lg.warn("%s egress denied (no rule): %s", job.label, strings.Join(ea.Denied, ", "))
	}

	meta := pairMeta{
		Agent: job.agentName, Harness: a.Harness, Model: a.Model, Effort: a.Effort, Task: t.Name,
		Sandbox: sbID, Image: sb.Image, Sudo: h.spec.Sudo,
		ExitCode: r.code, DurationS: round2(r.dur), Status: status, Error: errStr, Synced: &synced,
		PatsVersion:  version.Info.Version,
		DeniedEgress: ea.Denied, BlockedEgress: ea.Blocked, FailedEgress: ea.Failed,
		Summary:    summarize(a.Harness, filepath.Join(outDir, "stdout.log")),
		Components: sb.Components,
	}
	if sb.Driver == "lxd" {
		meta.LXD = lxdProvenanceOf(ctx, sb)
	}
	if h.spec.Egress.Set {
		meta.Egress = &egressRecord{
			Policy:   h.spec.Egress.Policy,
			Image:    proxyImageID(ctx, sb),
			Requests: ea.Requests, BytesIn: ea.In, BytesOut: ea.Out,
		}
	}
	// the ledger reads the summary's cost when the pair ends: set before
	// anything here can fail.
	if s := meta.Summary; s != nil {
		stat.tokens, stat.cost, stat.costKnown = s.OutputTokens, s.CostUSD, s.HasCost
	}
	// an interrupt during the close or the lookups above still makes the
	// pair interrupted.
	if ctx.Err() != nil {
		meta.Status, meta.Error = "error", "interrupted"
	}
	if job.meta != nil {
		job.meta(&meta)
	}
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
	stat.status = status
	stats := stat.cols(statScanner(a.Harness) != nil)
	if s := meta.Summary; s != nil {
		// cost + turns are omitted for harnesses whose stream doesn't report them.
		stats += fmt.Sprintf("  %s tok", humanK(s.OutputTokens))
		if s.HasCost {
			stats += fmt.Sprintf("  $%.2f", s.CostUSD)
		}
		if s.NumTurns > 0 {
			stats += fmt.Sprintf("  %d turns", s.NumTurns)
		}
	}
	lg.detail("%s %s", job.label, stats)
	return nil
}

// lxdProvenance is what a pair's metadata records of the lxd server and
// image it ran on.
type lxdProvenance struct {
	Remote           string `json:"remote"`
	InstanceType     string `json:"instance_type"` // container | virtual-machine
	ImageFingerprint string `json:"image_fingerprint"`
	ServerVersion    string `json:"server_version"`
}

// lxdProvenances caches each lxd sandbox's provenance for the run: one
// server round trip per sandbox, not per pair.
var lxdProvenances sync.Map

func lxdProvenanceOf(ctx context.Context, sb config.Sandbox) *lxdProvenance {
	if v, ok := lxdProvenances.Load(sb.Name + "\x00" + sb.Image); ok {
		return v.(*lxdProvenance)
	}
	h, err := hostFor(sb)
	if err != nil {
		return nil
	}
	t, err := h.Target(ctx)
	if err != nil {
		return nil
	}
	p := &lxdProvenance{Remote: strings.TrimPrefix(t.Key, "lxd:"), InstanceType: "container", ServerVersion: t.Version}
	if sb.VM {
		p.InstanceType = "virtual-machine"
	}
	if img, err := h.Resolve(ctx, sb.Image); err == nil {
		p.ImageFingerprint = img.ID
	}
	lxdProvenances.Store(sb.Name+"\x00"+sb.Image, p)
	return p
}

// stream is where -vv streams host scripts and the agent's stderr, beside
// their logs; nil below -vv.
func (o Options) stream() io.Writer {
	switch {
	case o.Verbose < 2:
		return nil
	case o.live != nil:
		return o.live
	}
	return o.Out
}

// runTotals is the run's last -v line: how its pairs ended, their output
// tokens and cost where the harness reports them, and the wall time.
func runTotals(stats []*pairStat, wall time.Duration) string {
	counts := map[string]int{}
	tokens, cost := 0, 0.0
	for _, s := range stats {
		if s.status == "" {
			continue // not started: interrupted first
		}
		counts[s.status]++
		tokens += s.tokens
		cost += s.cost
	}
	var parts []string
	for _, st := range []string{"ok", "nonzero", "timeout", "budget", "error"} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "no pair ran")
	}
	out := strings.Join(parts, ", ")
	if tokens > 0 {
		out += fmt.Sprintf("  %s tok", humanK(tokens))
	}
	if cost > 0 {
		out += fmt.Sprintf("  $%.2f", cost)
	}
	return out + "  " + wall.Round(100*time.Millisecond).String()
}

// runLogged runs f with its output written to logPath and, when tee is set,
// to tee too.
func runLogged(logPath string, tee io.Writer, f func(io.Writer) error) error {
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	var w io.Writer = lf
	if tee != nil {
		w = io.MultiWriter(lf, tee)
	}
	return f(w)
}

// homeMount is the in-sandbox HOME for a harness -- a writable dir the agent
// (running as an arbitrary uid) owns, where its creds + cache live.
const homeMount = "/pats-home"

type harnessSetup struct {
	dir     string // host side of homeMount
	mounts  []sandbox.Mount
	cleanup func()
}

// harnessHome gives a harness a writable HOME (the --user uid owns nothing),
// copies its file-based credentials when present, and warns when no supported
// credential source is available. copies are writable for token refreshes but
// discarded after the invocation; pats never modifies the host credential file.
// label ("agent/task") prefixes the warning.
func harnessHome(opts Options, label, harness string, env map[string]string, hasToken bool) (harnessSetup, error) {
	homeDir, err := sandbox.MkTemp("pats-home-")
	if err != nil {
		return harnessSetup{}, err
	}
	env["HOME"] = homeMount
	maps.Copy(env, agent.HomeEnv(harness, homeMount))

	hasCreds := hasToken
	if cf, rel := agent.HostCredsFile(harness); cf != "" {
		dst := filepath.Join(homeDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			sandbox.RemoveTemp(homeDir)
			return harnessSetup{}, err
		}
		if err := copyFile(cf, dst); err != nil {
			sandbox.RemoveTemp(homeDir)
			return harnessSetup{}, err
		}
		hasCreds = true
	}
	if !hasCreds {
		logw{opts.Out, opts.Color, opts.Verbose}.warn("%s no creds forwarded (expected %s); the harness may fail on auth", label, agent.CredentialHint(harness))
	}
	return harnessSetup{
		dir:     homeDir,
		mounts:  []sandbox.Mount{{Host: homeDir, Container: homeMount}},
		cleanup: func() { sandbox.RemoveTemp(homeDir) },
	}, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// egressFor renders the egress policy one agent run gets: the sandbox's, with
// the task's lists unioned in (the sandbox keeps every scalar), and under an
// allowlisting proxy the harness's own hosts (inference, token refresh), so
// pats.yaml only lists what the task needs. selfTest asks the driver to prove
// the sandbox has no route out first (preflight does).
func egressFor(sb config.Sandbox, te config.TaskEgress, harness, auditPath string, selfTest bool) sandbox.Egress {
	eg := sandbox.Egress{
		None:      sb.Egress.None,
		Set:       sb.Egress.Set,
		Policy:    sb.Egress.Policy,
		Image:     sb.ProxyImage,
		AuditPath: auditPath,
		SelfTest:  selfTest,
	}
	if !eg.Set {
		return eg
	}
	eg.Policy.Allow = mergeHosts(eg.Policy.Allow, te.Allow)
	eg.Policy.Deny = mergeHosts(eg.Policy.Deny, te.Deny)
	if eg.Policy.Default != "allow" {
		eg.Policy.Allow = mergeHosts(eg.Policy.Allow, agent.RequiredHosts[harness])
	}
	// the harness's hosts are exempt from the methods posture either way: its
	// inference traffic is not the task's to inspect.
	eg.Policy.Harness = mergeHosts(nil, agent.RequiredHosts[harness])
	return eg
}

// mergeHosts appends extras not already in hosts (exact string match; overlap
// via wildcards is harmless -- the proxy just sees a redundant entry).
//
// hosts is copied before appending: the caller passes cfg.Sandboxes[i].Egress.Allow,
// shared by every concurrent pair. today an in-place append is safe because
// yaml.v3 decodes sequences with cap == len (verified), so the append below
// always reallocates -- but nothing enforces that invariant, and a future
// caller (or yaml.v3 change) could silently start corrupting a sibling pair's
// egress policy.
func mergeHosts(hosts, extras []string) []string {
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	out := append([]string(nil), hosts...)
	for _, e := range extras {
		if !seen[e] {
			out = append(out, e)
		}
	}
	return out
}

// relToCwd renders a path relative to the working dir for display; falls back to
// the input if that's not possible (e.g. different volume).
func relToCwd(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil {
			return rel
		}
	}
	return p
}

// taskPrompt is the prompt handed to the agent: the literal prompt, the
// contents of prompt-file (resolved against the task's dir), or the stdout of
// prompt-cmd, a script run after stage with the task's env.
func taskPrompt(ctx context.Context, root, dir string, t config.Task, env map[string]string) ([]byte, error) {
	switch {
	case t.PromptFile != "":
		p := t.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return os.ReadFile(p)
	case t.PromptCmd != "":
		return step{script: t.PromptCmd, dir: dir}.output(ctx, root, env)
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

// nextRunDir creates and returns base/<nnn>-<yyyymmdd>-<adj>-<noun>: a
// globally monotonic counter (highest existing + 1), the date, and a friendly
// name derived deterministically from the numeric prefix. zero-padded to
// width 3 so lexical sort follows the counter.
func nextRunDir(base string, now time.Time) (string, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	date := now.UTC().Format("20060102")
	max := 0
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if _, n, ok := splitRunName(e.Name()); ok && n > max {
			max = n
		}
	}
	prefix := fmt.Sprintf("%03d-%s", max+1, date)
	dir := filepath.Join(base, prefix+"-"+generateName(prefix))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// best-effort "latest" symlink; relative target so .pats stays copyable
	link := filepath.Join(base, "latest")
	_ = os.Remove(link)
	_ = os.Symlink(filepath.Base(dir), link)
	return dir, nil
}

// writeJSON replaces path through a temp file and a rename, so a write cut
// short leaves the old file or none, never half of one.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// humanK renders a token count compactly: 412 -> "412", 9213 -> "9.2k".
func humanK(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func index[T any](xs []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(xs))
	for _, x := range xs {
		m[key(x)] = x
	}
	return m
}
