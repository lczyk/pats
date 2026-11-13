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
	Repeat  int     // runs of the selection, one after another; 0 or 1 is one
	Budget  float64 // the invocation's budget in us dollars; 0 is none

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
	opts.ledger = newLedger(opts.Budget)

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
		lg.warn("budget $%.2f reached at $%.2f: %s not started", opts.Budget, spent, what)
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
	slots := newSlots(cfg, jobs)
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
			defer slots.take(cfg, agents[p.Agent])()
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
				bar.start(label, stat, egressPath, cfg.HarnessOf(agents[p.Agent]).Parser() != nil)
			}
			job := taskJob(runDir, p, agents[p.Agent], tasks[p.Task])
			if err := runPair(ctx, cfg, po, runDir, job, sandboxes, stat); err != nil {
				logw{po.Out, opts.Color, opts.Verbose}.error("%s/%s %v", p.Agent, p.Task, err)
				pairFailed(ctx, cfg, job, sandboxes, stat, err)
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
