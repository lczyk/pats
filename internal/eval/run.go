// Package eval drives the run phase: expand the test-matrix and, per pair,
// prepare a sandbox, run the agent in it, and collect outputs into a run dir.
// (the score phase lands in a later phase.)
package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/sched"
	"github.com/lczyk/pats/src/sandbox"
)

const runsSubdir = ".pats/runs"

// Options configures a run.
type Options struct {
	ConfigDir string    // dir holding pats.yaml -- prompt/script paths resolve against it
	Now       time.Time // wall clock, for the run-dir slug (injectable for tests)
	Out       io.Writer // progress + host script output
	Jobs      int       // max concurrent pairs; 0 -> serial (1), negative -> auto (see resolveJobs)
	// StrictPreflight fails the run on any agent's failed preflight, in
	// place of leaving that agent's pairs out.
	StrictPreflight bool
	Color           bool     // colour log tags (set internally from Out's tty-ness)
	Verbose         int      // -v count: 1 adds detail lines, 2 streams host scripts and the agent's stderr
	Agents          []string // filter the pairs to these agents (empty -> all)
	Tasks           []string // filter the pairs to these tasks (empty -> all)
	Suites          []string // only expand these suites (empty -> all)
	Offline         bool     // skip the upstream checks, for a machine without a network
	// DryRun lists the pairs with a cost estimate and stops before the first
	// sandbox; JSON (with DryRun only) writes that list as json lines to
	// JSONOut, while Out takes the log.
	DryRun  bool
	JSON    bool
	JSONOut io.Writer
	Repeat  int           // runs of the selection, through one queue; 0 or 1 is one
	Budget  config.Budget // the invocation's caps; zero is none

	live io.Writer // -vv's stream when Out is a pair's buffer (see liveStream)
	// ledger is the invocation's spend (budget.md); nil counts nothing.
	ledger *ledger
	// prices is what the catalogue says each agent's model costs, for a
	// harness that reports tokens and no cost; nil offline.
	prices map[string]*modelPrice
	// clock is what the dispatcher times pairs by; nil is time.Now.
	clock func() time.Time
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
	// a file outside the project changes what this run does: say so.
	if n := cfg.Harnesses.Note(); n != "" {
		lg.info("%s", n)
	}
	suites, err := cfg.RunSuites(opts.Suites)
	if err != nil {
		return "", err
	}
	pairs, err := cfg.ExpandTestPairs(suites...)
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
		if manual := cfg.ManualSuitesWith(opts.Agents, opts.Tasks); len(opts.Suites) == 0 && len(manual) > 0 {
			what := "manual suite " + manual[0] + " holds it"
			if len(manual) > 1 {
				what = "manual suites " + strings.Join(manual, ", ") + " hold it"
			}
			return "", fmt.Errorf("%w (%s: add -S %s)", err, what, manual[0])
		}
		return "", err
	}
	// first output up front: the slow startup steps (image build, preflight) are
	// otherwise silent, so a bare `pats run` looks hung until the first pair logs.
	lg.detail("%d pair(s) to run", len(pairs))
	// behind the machine's own proxy every sandbox goes through it: said
	// before the first pair, so a run behind a proxy is never a surprise.
	mp, err := machineProxy()
	if err != nil {
		return "", fmt.Errorf("the machine's proxy env: %w", err)
	}
	if mp != nil {
		lg.info("egress via upstream %s", mp.Chain.Describe())
	}
	warnUnboxed(cfg, pairs, lg)
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
	checks, err := usePins(ctx, cfg, opts, pairs, nil)
	if err != nil {
		return "", err
	}
	// a model openrouter does not have, or one without tools, fails here,
	// before the run dir and any sandbox.
	if !opts.Offline {
		prices, warns, err := checkModels(ctx, catalogueAgents(cfg, pairAgents(cfg, pairs), "openrouter"), http.DefaultClient)
		if err != nil {
			return "", err
		}
		opts.prices = prices
		for _, w := range warns {
			lg.warn("%s", w)
		}
	}
	// what dead runs of this machine left on these places goes first: a
	// server held by a killed run's sandbox has less room for this one.
	sweepDead(ctx, cfg, sweepNames(cfg, pairs), opts.DryRun, lg)
	if opts.DryRun {
		return "", dryRun(cfg, opts, pairs, lg)
	}
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.Name })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })
	// past runs' durations, for the dispatch order: a run never fails over
	// its history, so whatever cannot be read is left out.
	want := map[historyKey]bool{}
	for _, p := range pairs {
		want[keyOf(p, agents[p.Agent])] = true
	}
	hist, _ := readHistory(filepath.Join(opts.ConfigDir, runsSubdir), want, false, lg)

	// newer upstream images, looked up while preflight runs: a warning
	// each, never a stop.
	upstream := make(chan []string, 1)
	if opts.Offline {
		upstream <- nil
	} else {
		go func() { upstream <- checkUpstream(ctx, checks, upstreamBudget) }()
	}

	// the step sandboxes' check first and alone: seconds, and a pair's stage
	// may run in one.
	if err := checkStepBoxes(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}
	// the credential checks run at once with each other and with the first
	// pairs' setup; no agent starts until every check has settled
	// (docs/spec/preflight-overlap.md). the budget spans every repeat: the
	// invocation's. it admits pairs from the start, and its clock starts at
	// the gate.
	lg.detail("preflight: checking agent credentials")
	opts.ledger = newUnstartedLedger(opts.Budget)
	g := newPreflightGate()
	jobs := resolveJobs(opts.Jobs, func() int { return autoCPUs(ctx, cfg, pairs) })

	// the repeats (docs/spec/repeats.md): each an ordinary run with its own
	// run dir, all through one queue; what is above happens once for all.
	n := max(opts.Repeat, 1)
	d := newDispatch(cfg, opts, lg, pairs, n, jobs, agents, tasks, sandboxes, hist)
	d.gate = g
	d.openBar()
	// the checks log beside the pairs: through the dispatcher's own writer.
	pre := opts
	pre.Out = d.writer()
	plg := logw{pre.Out, pre.Color, pre.Verbose}
	type checked struct {
		failures []agentFailure
		dropped  int
		fatal    error
	}
	settled := make(chan checked, 1)
	go func() {
		var c checked
		failures, err := checkAgents(ctx, cfg, pre, pairs, true)
		if err != nil {
			// interrupted: the gate never opens, and every pair waiting at
			// it is discarded as its context ends.
			settled <- c
			return
		}
		c.failures = failures
		_, c.dropped, c.fatal = dropFailed(pairs, failures, opts.StrictPreflight)
		out := map[string]bool{}
		for _, f := range failures {
			out[f.agent] = true
		}
		now := time.Now()
		if c.fatal == nil {
			opts.ledger.startClock(now)
		}
		g.settle(out, c.fatal != nil, now)
		for _, w := range <-upstream {
			plg.warn("%s", w)
		}
		settled <- c
	}()

	runDir, dirErr := d.run(ctx)
	c := <-settled
	d.closeBar()
	// said once, now that the pairs they stopped have stopped; a fatal
	// one's error names them all.
	if c.fatal == nil {
		for _, f := range c.failures {
			lg.error("%v", f)
		}
	}
	failures := c.failures
	if c.fatal != nil {
		if ctx.Err() != nil {
			return runDir, ErrInterrupted
		}
		if dirErr != nil {
			return runDir, dirErr
		}
		return runDir, c.fatal
	}
	// every stop is said, whichever error the run returns.
	if len(failures) > 0 {
		out := map[string]bool{}
		for _, f := range failures {
			out[f.agent] = true
		}
		d.uncount(out)
		lg.warn("%s", droppedText(failures, c.dropped*n, n))
	}
	// a run dir's error wins, but a cap reached is still said.
	if d.budgetStop || (dirErr != nil && opts.ledger.reached()) {
		lg.warn("%s: %s not started", opts.ledger.reachedAt(), d.notStartedText())
	}
	if ctx.Err() != nil {
		return runDir, ErrInterrupted
	}
	switch {
	case dirErr != nil:
		return runDir, dirErr
	case d.budgetStop:
		return runDir, ErrBudget
	case len(failures) > 0:
		return runDir, preflightErr(failures)
	}
	return runDir, nil
}

// ErrPreflight is a run that left out the pairs of agents whose preflight
// failed (docs/spec/preflight-per-agent.md).
var ErrPreflight = errors.New("preflight failed")

// preflightErr is ErrPreflight with every failure under it.
func preflightErr(failures []agentFailure) error {
	errs := make([]error, len(failures))
	for i, f := range failures {
		errs[i] = f
	}
	return fmt.Errorf("%w: %w", ErrPreflight, errors.Join(errs...))
}

// dropFailed takes the pairs of the agents whose preflight failed out of
// the selection, and says how many it took. a failure of the invocation's,
// any failure under strict, or every agent failing fails the run instead.
func dropFailed(pairs []config.TestPair, failures []agentFailure, strict bool) ([]config.TestPair, int, error) {
	if len(failures) == 0 {
		return pairs, 0, nil
	}
	out := map[string]bool{}
	fatal := strict
	for _, f := range failures {
		out[f.agent] = true
		fatal = fatal || f.kind == failInvocation
	}
	var kept []config.TestPair
	for _, p := range pairs {
		if !out[p.Agent] {
			kept = append(kept, p)
		}
	}
	if fatal || len(kept) == 0 {
		return nil, 0, preflightErr(failures)
	}
	return kept, len(pairs) - len(kept), nil
}

// droppedText is the closing line for the agents a failed preflight left
// out: n is every repeat's count of their pairs.
func droppedText(failures []agentFailure, n, repeats int) string {
	var names []string
	for _, f := range failures {
		names = append(names, fmt.Sprintf("%s (%s)", f.agent, f.kind))
	}
	s := fmt.Sprintf("preflight failed for %s: %d pair(s) not started", strings.Join(names, ", "), n)
	if repeats > 1 {
		s += fmt.Sprintf(" over %d runs", repeats)
	}
	return s
}

// ErrInterrupted is a run stopped by ctrl-c or sigterm.
var ErrInterrupted = errors.New("interrupted")

// repeatRun is one repeat as the dispatcher runs it.
type repeatRun struct {
	dir        string // made when its first pair starts; "" before
	kept       int    // its pairs recorded: not discarded at the gate
	pending    int    // its pairs not yet settled: queued or running
	notStarted int    // its pairs a stop left in the queue
	// stoppedAgents are the agents of those pairs, so a gate that settles
	// after the stop can take its dropped agents' back: the preflight's.
	stoppedAgents []string
	stats         []*pairStat
	first, last   time.Time
}

// repeatPair is a pair of one repeat: what the scheduler's tasks carry.
type repeatPair struct {
	rep  int // 1-based
	pair config.TestPair
}

type pairTask = sched.Task[historyKey, repeatPair]

// dispatch runs every pair of every repeat through one scheduler
// (internal/sched), up to jobs at a time, longest first. one goroutine, run,
// starts every pair; a single run is the same with one repeat.
type dispatch struct {
	cfg       *config.Config
	opts      Options
	lg        logw
	n, jobs   int
	agents    map[string]config.Agent
	tasks     map[string]config.Task
	sandboxes map[string]config.Sandbox

	s       *sched.Scheduler[historyKey, repeatPair]
	now     func() time.Time
	reps    []*repeatRun // index 0 is repeat 1
	labelW  int
	bar     *progress
	outMu   sync.Mutex
	lastDir string

	budgetStop bool // a reached budget left pairs in the queue

	barOpen bool // openBar ran

	// gate holds each pair between its setup and its agent until the
	// credential checks settle; nil for none.
	gate *preflightGate
	// localErr is the first error of the run dir's own: a discarded pair's
	// dir not removed, a write a discarded pair met.
	localMu  sync.Mutex
	localErr error
}

// writer is a writer of whole lines beside the dispatcher's own: above the
// bar when there is one, else under its lock.
func (d *dispatch) writer() io.Writer { return sayWriter{d} }

type sayWriter struct{ d *dispatch }

func (w sayWriter) Write(b []byte) (int, error) {
	d := w.d
	if d.bar != nil {
		d.bar.log(strings.TrimRight(string(b), "\n"))
		return len(b), nil
	}
	d.outMu.Lock()
	defer d.outMu.Unlock()
	return d.opts.Out.Write(b)
}

// uncount takes the pairs of agents the gate dropped out of what a stop
// counted as not started: the preflight's line counts them.
func (d *dispatch) uncount(dropped map[string]bool) {
	for _, rr := range d.reps {
		for _, a := range rr.stoppedAgents {
			if dropped[a] {
				rr.notStarted--
			}
		}
		rr.stoppedAgents = nil
	}
}

// local keeps the first of the run dir's own errors.
func (d *dispatch) local(err error) {
	d.localMu.Lock()
	defer d.localMu.Unlock()
	if d.localErr == nil {
		d.localErr = err
	}
}

// ended is a started pair coming back to the dispatcher.
type ended struct {
	t    *pairTask
	stat *pairStat
}

func newDispatch(cfg *config.Config, opts Options, lg logw, pairs []config.TestPair, n, jobs int,
	agents map[string]config.Agent, tasks map[string]config.Task, sandboxes map[string]config.Sandbox,
	hist map[historyKey]*pastPairs,
) *dispatch {
	caps := map[string]int{}
	for _, sb := range cfg.Sandboxes {
		if sb.MaxParallel > 0 {
			caps[sb.Name] = sb.MaxParallel
		}
	}
	d := &dispatch{
		cfg: cfg, opts: opts, lg: lg, n: n, jobs: jobs,
		agents: agents, tasks: tasks, sandboxes: sandboxes,
		s: sched.New[historyKey, repeatPair](sched.Options{Slots: jobs, Caps: caps}), now: opts.clock,
	}
	if d.now == nil {
		d.now = time.Now
	}
	for k, h := range hist {
		for _, sec := range h.durations {
			d.s.Prime(k, time.Duration(sec*float64(time.Second)))
		}
	}
	known := 0
	for r := 1; r <= n; r++ {
		d.reps = append(d.reps, &repeatRun{pending: len(pairs)})
		for i, p := range pairs {
			a := agents[p.Agent]
			sb, _ := cfg.ResolveSandbox(a)
			t := &pairTask{
				Key: keyOf(p, a), Group: sb, Round: r, Index: i,
				Limit: effectiveTimeout(a, tasks[p.Task]), Value: repeatPair{r, p},
			}
			d.s.Add(t)
			d.labelW = max(d.labelW, len(d.label(t)))
			if r == 1 && d.s.Primed(t.Key) {
				known++
			}
		}
	}
	lg.detail("dispatch: longest first, history for %d of %d pair(s)", known, len(pairs))
	return d
}

// effectiveTimeout is how long the agent may run: the task's timeout,
// shortened by the agent's time budget; 0 is no limit.
func effectiveTimeout(a config.Agent, t config.Task) time.Duration {
	tt := t.TimeoutDuration()
	if bt := a.Budget.Time; bt > 0 && (tt == 0 || bt < tt) {
		return bt
	}
	return tt
}

// label is a pair's name in its log lines and on the bar: its repeat too
// when there is more than one.
func (d *dispatch) label(t *pairTask) string {
	l := t.Value.pair.Agent + "/" + t.Value.pair.Task
	if d.n > 1 {
		l += "#" + strconv.Itoa(t.Value.rep)
	}
	return l
}

// say logs a line of the invocation's own, above the bar when there is one;
// a detail line only under -v.
func (d *dispatch) say(lv level, detail bool, format string, args ...any) {
	if detail && d.lg.verbose < 1 {
		return
	}
	s := d.lg.line(lv, format, args...)
	if d.bar != nil {
		d.bar.log(s)
		return
	}
	d.outMu.Lock()
	defer d.outMu.Unlock()
	fmt.Fprintln(d.opts.Out, s)
}

// run starts every pair it can and waits for every started one to end. it
// returns the newest run dir made, and the error a run dir that could not
// be made stopped it with.
// openBar draws the progress region on a tty (total bar + a live line per
// in-flight pair); otherwise (pipe/ci/tee) per-pair lines are plain. it is
// up before anything writes beside the pairs, and closeBar takes it down
// once nothing does.
func (d *dispatch) openBar() {
	if d.barOpen {
		return
	}
	d.barOpen = true
	if total := d.s.Queued(); isProgressTTY(d.opts.Out) {
		d.bar = newProgress(d.opts.Out, total, d.labelW)
	} else if d.jobs > 1 {
		d.lg.detail("running %d pairs, up to %d in parallel", total, d.jobs)
	}
}

func (d *dispatch) closeBar() {
	if d.bar != nil {
		d.bar.close()
	}
}

func (d *dispatch) run(ctx context.Context) (string, error) {
	if !d.barOpen {
		d.openBar()
		defer d.closeBar()
	}
	// each agent's pairs run under a context of its own: the gate dropping
	// the agent ends its setups where they are.
	agentCtx := map[string]context.Context{}
	var cancels []context.CancelFunc
	cancelOf := map[string]context.CancelFunc{}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	ctxOf := func(agent string) context.Context {
		if c, ok := agentCtx[agent]; ok {
			return c
		}
		c, cancel := context.WithCancel(ctx)
		agentCtx[agent], cancelOf[agent] = c, cancel
		cancels = append(cancels, cancel)
		return c
	}

	done := make(chan ended)
	stopped := false
	var dirErr error
	gs := d.gate.settled()
	var onSettle func()
	stop := func() {
		// a gate that has settled takes its dropped agents' pairs first:
		// they are the preflight's to count, not the budget's.
		if gs != nil {
			select {
			case <-gs:
				onSettle()
			default:
			}
		}
		stopped = true
		dropped := d.s.Drop(d.now())
		for _, t := range dropped {
			d.reps[t.Value.rep-1].notStarted++
			d.reps[t.Value.rep-1].stoppedAgents = append(d.reps[t.Value.rep-1].stoppedAgents, t.Value.pair.Agent)
			d.settle(t.Value.rep)
		}
		if d.bar != nil {
			d.bar.drop(len(dropped))
		}
	}
	// back counts a pair that came back; drain does so for every pair that
	// has ended by now, so one still waiting to say so does not rank as
	// running at the next pick.
	back := func(e ended) {
		rr := d.reps[e.t.Value.rep-1]
		if e.stat.discarded {
			// its slot goes back; nothing of it is learnt or counted.
			d.s.Done(e.t, 0, false)
			d.settle(e.t.Value.rep)
			return
		}
		ok := e.stat.status != "" && e.stat.status != "error" && e.stat.status != "infra"
		d.s.Done(e.t, time.Duration(e.stat.agentDur*float64(time.Second)), ok)
		rr.kept++
		rr.stats = append(rr.stats, e.stat)
		rr.last = d.now()
		d.settle(e.t.Value.rep)
	}
	// the gate settling takes the queued pairs of the agents it dropped out,
	// or every queued pair when it is fatal: not started, and no budget's.
	onSettle = func() {
		gs = nil
		g := d.gate
		for agent, cancel := range cancelOf {
			if !g.passes(agent) {
				cancel()
			}
		}
		var dropped []*pairTask
		if g.fatal {
			stopped = true
			dropped = d.s.Drop(d.now())
		} else {
			dropped = d.s.DropIf(func(t *pairTask) bool { return g.dropped[t.Value.pair.Agent] })
		}
		for _, t := range dropped {
			d.settle(t.Value.rep)
		}
		if d.bar != nil {
			d.bar.drop(len(dropped))
		}
	}
	drain := func() {
		for {
			select {
			case e := <-done:
				back(e)
			default:
				return
			}
		}
	}
	for {
		for !stopped {
			if gs != nil {
				select {
				case <-gs:
					onSettle()
					continue
				default:
				}
			}
			drain()
			if ctx.Err() != nil {
				d.say(lvWarn, false, "interrupted -- stopping")
				stop()
				break
			}
			t := d.s.Peek(d.now())
			if t == nil {
				break // no slot free, nothing queued, or all wait on a cap
			}
			stat := &pairStat{}
			if !d.opts.ledger.admit(stat) {
				d.budgetStop = true
				stop()
				break
			}
			rep := t.Value.rep
			rr := d.reps[rep-1]
			if rr.dir == "" {
				dir, err := makeRunDir(filepath.Join(d.opts.ConfigDir, runsSubdir), d.opts.Now)
				if err != nil {
					d.opts.ledger.finish(stat)
					dirErr = err
					stop()
					break
				}
				rr.dir, rr.first, d.lastDir = dir, d.now(), dir
				if d.n > 1 {
					d.say(lvInfo, false, "run %d/%d: %s", rep, d.n, filepath.Base(dir))
				} else {
					d.say(lvInfo, true, "run dir: %s", relToCwd(dir))
				}
			}
			if !d.s.Start(t, d.now()) {
				// it was just peeked by this goroutine, so it cannot be
				// refused; if it were, it would not run.
				d.opts.ledger.finish(stat)
				break
			}
			actx := ctxOf(t.Value.pair.Agent)
			if d.gate == nil {
				actx = ctx
			}
			go func() {
				d.runOne(actx, t, rr.dir, stat)
				done <- ended{t, stat}
			}()
		}
		if d.s.Running() == 0 {
			break
		}
		var e ended
		if stopped {
			// a stop still waits on the gate: its settling cancels the
			// setups of the agents it drops.
			select {
			case e = <-done:
			case <-gs:
				onSettle()
				continue
			}
		} else {
			select {
			case e = <-done:
			case <-gs:
				onSettle()
				continue
			case <-ctx.Done():
				d.say(lvWarn, false, "interrupted -- stopping")
				stop()
				continue
			}
		}
		back(e)
	}
	// a repeat whose every pair was discarded leaves no dir: the next run
	// takes its number.
	d.lastDir = ""
	for _, rr := range d.reps {
		if rr.dir == "" {
			continue
		}
		if rr.kept == 0 {
			if err := os.RemoveAll(rr.dir); err != nil {
				d.local(fmt.Errorf("remove run dir %s, where no agent started: %w", rr.dir, err))
				d.lastDir = rr.dir
			}
			continue
		}
		d.lastDir = rr.dir
	}
	if dirErr == nil {
		dirErr = d.localErr
	}
	return d.lastDir, dirErr
}

// settle counts one pair of repeat rep as settled, and logs the repeat's
// totals once its last has.
func (d *dispatch) settle(rep int) {
	rr := d.reps[rep-1]
	rr.pending--
	if rr.pending > 0 || rr.dir == "" || rr.kept == 0 {
		return
	}
	sum := runTotals(rr.stats, rr.last.Sub(rr.first))
	if d.n > 1 {
		sum = fmt.Sprintf("run %d/%d: %s", rep, d.n, sum)
	}
	d.say(lvInfo, true, "%s", sum)
}

// runOne runs a started pair into its run dir.
func (d *dispatch) runOne(ctx context.Context, t *pairTask, runDir string, stat *pairStat) {
	defer d.opts.ledger.finish(stat)
	label, p := d.label(t), t.Value.pair
	po := d.opts
	var buf bytes.Buffer
	if d.bar != nil || d.jobs > 1 {
		po.Out = &buf
		if d.opts.Verbose >= 2 {
			ls := liveStream(label, d.bar, d.opts.Out, &d.outMu)
			defer ls.flush()
			po.live = ls
		}
	} else {
		// one pair at a time writes straight out, beside the dispatcher's
		// own lines (an interrupt's), so under the same lock. -vv's stream
		// says which repeat when there are several.
		po.Out = &lockedWriter{mu: &d.outMu, w: d.opts.Out}
		if d.opts.Verbose >= 2 && d.n > 1 {
			ls := liveStream(label, nil, d.opts.Out, &d.outMu)
			defer ls.flush()
			po.live = ls
		}
	}
	a := d.agents[p.Agent]
	if d.bar != nil {
		egressPath := filepath.Join(runDir, p.Agent, p.Task, "egress.log")
		d.bar.start(label, stat, egressPath, d.cfg.HarnessOf(a).Parser() != nil)
	}
	job := taskJob(runDir, p, a, d.tasks[p.Task])
	job.label, job.gate = label, d.gate
	err := runPair(ctx, d.cfg, po, runDir, job, d.sandboxes, stat)
	discard := errors.Is(err, errDiscard)
	// a pair that failed before the gate is recorded only once its agent
	// passed it; a write of the run dir's own that failed is the run's
	// error either way.
	if err != nil && !discard && !d.gate.pass(ctx, p.Agent) {
		discard = true
	}
	if discard && errors.Is(err, errLocal) {
		d.local(fmt.Errorf("%s: %w", label, err))
	}
	if discard {
		stat.discarded = true
		if rerr := os.RemoveAll(job.outDir); rerr != nil {
			d.local(fmt.Errorf("%s: remove its pair dir: %w", label, rerr))
		}
		_ = os.Remove(filepath.Dir(job.outDir)) // the agent's dir, once empty
		if d.bar != nil {
			d.bar.discard(label)
		}
		return
	}
	if err != nil {
		logw{po.Out, d.opts.Color, d.opts.Verbose}.error("%s %v", label, err)
		pairFailed(ctx, d.cfg, job, d.sandboxes, stat, err)
	}
	switch {
	case d.bar != nil:
		d.bar.finish(label, buf.String())
	case d.jobs > 1:
		d.outMu.Lock()
		d.opts.Out.Write(buf.Bytes())
		d.outMu.Unlock()
	}
}

// notStartedText is what a budget stop left: the pairs of the repeats begun,
// and the repeats never begun.
func (d *dispatch) notStartedText() string {
	pairs, unbegun, later := 0, 0, 0 // later: the pairs of the repeats not begun
	lo, hi := 0, 0                   // the repeats begun with pairs not started
	for i, rr := range d.reps {
		switch {
		case rr.dir == "":
			unbegun++
			later += rr.notStarted
		case rr.notStarted > 0:
			pairs += rr.notStarted
			if lo == 0 {
				lo = i + 1
			}
			hi = i + 1
		}
	}
	switch {
	case pairs == 0 && unbegun == d.n:
		return fmt.Sprintf("%d run(s) (%d pair(s))", unbegun, later) // a cap reached before the first
	case pairs == 0:
		return fmt.Sprintf("%d more run(s) (%d pair(s))", unbegun, later)
	}
	what := fmt.Sprintf("%d pair(s)", pairs)
	switch {
	case d.n == 1:
	case lo == hi:
		what += fmt.Sprintf(" of run %d/%d", lo, d.n)
	default:
		what += fmt.Sprintf(" of runs %d-%d/%d", lo, hi, d.n)
	}
	if unbegun > 0 {
		what += fmt.Sprintf(" and %d more run(s) (%d pair(s))", unbegun, later)
	}
	return what
}

// lockedWriter writes to w under mu.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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
		if used[sb.Name] && (pick == nil || (sb.Driver == sandbox.LXDDriver && pick.Driver != sandbox.LXDDriver)) {
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

// machineProxy is the machine's proxy as pats found it; a var so tests set
// one.
var machineProxy = sandbox.Machine

// slots bounds the agent calls running at once: -j run-wide, and a
// sandbox's max-parallel under it. a call takes its sandbox's slot before a
// run-wide one, so calls queued on a capped sandbox never hold a slot
// another sandbox could use.
type slots struct {
	all     chan struct{}
	sandbox map[string]chan struct{}
}

func newSlots(cfg *config.Config, jobs int) *slots {
	s := &slots{all: make(chan struct{}, jobs), sandbox: map[string]chan struct{}{}}
	for _, sb := range cfg.Sandboxes {
		if sb.MaxParallel > 0 {
			s.sandbox[sb.Name] = make(chan struct{}, sb.MaxParallel)
		}
	}
	return s
}

// take waits for a's slots and returns what gives them back.
func (s *slots) take(cfg *config.Config, a config.Agent) (release func()) {
	var own chan struct{}
	if id, err := cfg.ResolveSandbox(a); err == nil {
		own = s.sandbox[id]
	}
	if own != nil {
		own <- struct{}{}
	}
	s.all <- struct{}{}
	return func() {
		<-s.all
		if own != nil {
			<-own
		}
	}
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
	for _, st := range []string{"ok", "nonzero", "timeout", "budget", "infra", "error"} {
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

// makeRunDir is nextRunDir, or a test's stand-in.
var makeRunDir = nextRunDir

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
