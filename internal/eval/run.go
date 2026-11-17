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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
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
	Repeat  int           // runs of the selection, through one queue; 0 or 1 is one
	Budget  config.Budget // the invocation's caps; zero is none

	live io.Writer // -vv's stream when Out is a pair's buffer (see liveStream)
	// ledger is the invocation's spend (budget.md); nil counts nothing.
	ledger *ledger
	// prices is what the catalogue says each agent's model costs, for a
	// harness that reports tokens and no cost; nil offline.
	prices map[string]*modelPrice
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

	// fail fast on bad/expired creds before spinning per-pair containers.
	lg.detail("preflight: checking agent credentials")
	if err := Preflight(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}
	// the budget spans every repeat: the invocation's. its clock starts
	// here, at the end of preflight.
	opts.ledger = newLedger(opts.Budget)
	for _, w := range <-upstream {
		lg.warn("%s", w)
	}

	jobs := resolveJobs(opts.Jobs, func() int { return autoCPUs(ctx, cfg, pairs) })

	// the repeats (docs/spec/repeats.md): each an ordinary run with its own
	// run dir, all through one queue; what is above happens once for all.
	n := max(opts.Repeat, 1)
	d := newDispatch(cfg, opts, lg, pairs, n, jobs, agents, tasks, sandboxes, hist)
	runDir, dirErr := d.run(ctx)
	if ctx.Err() != nil {
		return runDir, ErrInterrupted
	}
	if d.budgetStop {
		lg.warn("%s: %s not started", opts.ledger.reachedAt(), d.notStartedText())
	}
	switch {
	case dirErr != nil:
		return runDir, dirErr
	case d.budgetStop:
		return runDir, ErrBudget
	}
	return runDir, nil
}

// ErrInterrupted is a run stopped by ctrl-c or sigterm.
var ErrInterrupted = errors.New("interrupted")

// repeatRun is one repeat as the dispatcher runs it.
type repeatRun struct {
	dir         string // made when its first pair starts; "" before
	pending     int    // its pairs not yet settled: queued or running
	notStarted  int    // its pairs a stop left in the queue
	stats       []*pairStat
	first, last time.Time
}

// dispatch runs every pair of every repeat through one queue, up to jobs at
// a time, longest first (dispatch.go). one goroutine, run, starts every pair;
// a single run is the same with one repeat.
type dispatch struct {
	cfg       *config.Config
	opts      Options
	lg        logw
	n, jobs   int
	agents    map[string]config.Agent
	tasks     map[string]config.Task
	sandboxes map[string]config.Sandbox
	caps      map[string]int // a sandbox's max_parallel, where it has one

	queue   []*queued
	reps    []*repeatRun // index 0 is repeat 1
	est     *estimates
	bar     *progress
	outMu   sync.Mutex
	lastDir string

	budgetStop bool // a reached budget left pairs in the queue
}

// ended is a started pair coming back to the dispatcher.
type ended struct {
	q     *queued
	stat  *pairStat
	start time.Time
}

func newDispatch(cfg *config.Config, opts Options, lg logw, pairs []config.TestPair, n, jobs int,
	agents map[string]config.Agent, tasks map[string]config.Task, sandboxes map[string]config.Sandbox,
	hist map[historyKey]*pastPairs,
) *dispatch {
	d := &dispatch{
		cfg: cfg, opts: opts, lg: lg, n: n, jobs: jobs,
		agents: agents, tasks: tasks, sandboxes: sandboxes, caps: map[string]int{},
		est: newEstimates(hist),
	}
	for _, sb := range cfg.Sandboxes {
		if sb.MaxParallel > 0 {
			d.caps[sb.Name] = sb.MaxParallel
		}
	}
	for r := 1; r <= n; r++ {
		d.reps = append(d.reps, &repeatRun{pending: len(pairs)})
		for i, p := range pairs {
			a := agents[p.Agent]
			sb, _ := cfg.ResolveSandbox(a)
			d.queue = append(d.queue, &queued{
				rep: r, idx: i, pair: p, key: keyOf(p, a), sandbox: sb,
				limit: effectiveTimeout(a, tasks[p.Task]),
			})
		}
	}
	known := 0
	for _, p := range pairs {
		if len(d.est.hist[keyOf(p, agents[p.Agent])]) > 0 {
			known++
		}
	}
	lg.detail("dispatch: longest first, history for %d of %d pair(s)", known, len(pairs))
	return d
}

// label is a pair's name in its log lines and on the bar: its repeat too
// when there is more than one.
func (d *dispatch) label(q *queued) string {
	l := q.pair.Agent + "/" + q.pair.Task
	if d.n > 1 {
		l += "#" + strconv.Itoa(q.rep)
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
func (d *dispatch) run(ctx context.Context) (string, error) {
	// on a tty, draw a sticky progress region (total bar + a live line per
	// in-flight pair); otherwise (pipe/ci/tee) print plain per-pair lines.
	if isProgressTTY(d.opts.Out) {
		labelW := 0
		for _, q := range d.queue {
			labelW = max(labelW, len(d.label(q)))
		}
		d.bar = newProgress(d.opts.Out, len(d.queue), labelW)
		defer d.bar.close()
	} else if d.jobs > 1 {
		d.lg.detail("running %d pairs, up to %d in parallel", len(d.queue), d.jobs)
	}

	done := make(chan ended)
	running := 0
	perSandbox := map[string]int{}
	stopped := false
	var dirErr error
	stop := func() {
		stopped = true
		for _, q := range d.queue {
			rr := d.reps[q.rep-1]
			rr.notStarted++
			d.settle(q.rep)
		}
		if d.bar != nil {
			d.bar.drop(len(d.queue))
		}
		d.queue = nil
	}
	for {
		for !stopped && running < d.jobs && len(d.queue) > 0 {
			if ctx.Err() != nil {
				d.say(lvWarn, false, "interrupted -- stopping")
				stop()
				break
			}
			now := time.Now()
			d.est.order(d.queue, now)
			i := slices.IndexFunc(d.queue, func(q *queued) bool {
				c, capped := d.caps[q.sandbox]
				return !capped || perSandbox[q.sandbox] < c
			})
			if i < 0 {
				break // every queued pair waits on its sandbox's cap
			}
			q := d.queue[i]
			stat := &pairStat{}
			if !d.opts.ledger.admit(stat) {
				d.budgetStop = true
				stop()
				break
			}
			rr := d.reps[q.rep-1]
			if rr.dir == "" {
				dir, err := nextRunDir(filepath.Join(d.opts.ConfigDir, runsSubdir), d.opts.Now)
				if err != nil {
					d.opts.ledger.finish(stat)
					dirErr = err
					stop()
					break
				}
				rr.dir, rr.first, d.lastDir = dir, now, dir
				if d.n > 1 {
					d.say(lvInfo, false, "run %d/%d: %s", q.rep, d.n, filepath.Base(dir))
				} else {
					d.say(lvInfo, true, "run dir: %s", relToCwd(dir))
				}
			}
			d.queue = slices.Delete(d.queue, i, i+1)
			running++
			perSandbox[q.sandbox]++
			d.est.start(q.key, now)
			go func() {
				d.runOne(ctx, q, rr.dir, stat)
				done <- ended{q, stat, now}
			}()
		}
		if running == 0 {
			break
		}
		var e ended
		if stopped {
			e = <-done
		} else {
			select {
			case e = <-done:
			case <-ctx.Done():
				d.say(lvWarn, false, "interrupted -- stopping")
				stop()
				continue
			}
		}
		running--
		perSandbox[e.q.sandbox]--
		d.est.end(e.q.key, e.start, e.stat.status, e.stat.agentDur)
		rr := d.reps[e.q.rep-1]
		rr.stats = append(rr.stats, e.stat)
		rr.last = time.Now()
		d.settle(e.q.rep)
	}
	return d.lastDir, dirErr
}

// settle counts one pair of repeat rep as settled, and logs the repeat's
// totals once its last has.
func (d *dispatch) settle(rep int) {
	rr := d.reps[rep-1]
	rr.pending--
	if rr.pending > 0 || rr.dir == "" {
		return
	}
	sum := runTotals(rr.stats, rr.last.Sub(rr.first))
	if d.n > 1 {
		sum = fmt.Sprintf("run %d/%d: %s", rep, d.n, sum)
	}
	d.say(lvInfo, true, "%s", sum)
}

// runOne runs a started pair into its run dir.
func (d *dispatch) runOne(ctx context.Context, q *queued, runDir string, stat *pairStat) {
	defer d.opts.ledger.finish(stat)
	label := d.label(q)
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
		// own lines (an interrupt's), so under the same lock.
		po.Out = &lockedWriter{mu: &d.outMu, w: d.opts.Out}
	}
	a := d.agents[q.pair.Agent]
	if d.bar != nil {
		egressPath := filepath.Join(runDir, q.pair.Agent, q.pair.Task, "egress.log")
		d.bar.start(label, stat, egressPath, d.cfg.HarnessOf(a).Parser() != nil)
	}
	job := taskJob(runDir, q.pair, a, d.tasks[q.pair.Task])
	job.label = label
	if err := runPair(ctx, d.cfg, po, runDir, job, d.sandboxes, stat); err != nil {
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
	pairs, unbegun := 0, 0
	lo, hi := 0, 0 // the repeats begun with pairs not started
	for i, rr := range d.reps {
		switch {
		case rr.dir == "":
			unbegun++
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
		return fmt.Sprintf("%d run(s)", unbegun) // a cap reached before the first
	case pairs == 0:
		return fmt.Sprintf("%d more run(s)", unbegun)
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
		what += fmt.Sprintf(" and %d more run(s)", unbegun)
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
