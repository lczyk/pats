// Package eval drives the run phase: expand the test-matrix and, per pair,
// prepare a sandbox, run the agent in it, and collect outputs into a run dir.
// (the score phase lands in a later phase.)
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
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
)

const runsSubdir = ".pats/runs"

// Options configures a run.
type Options struct {
	ConfigDir string    // dir holding pats.yaml -- prompt/script paths resolve against it
	Now       time.Time // wall clock, for the run-dir slug (injectable for tests)
	Out       io.Writer // progress + host script output
	Jobs      int       // max concurrent pairs; 0 -> serial (1), negative -> auto (see resolveJobs)
	Color     bool      // colour log tags (set internally from Out's tty-ness)
	Agents    []string  // filter the pairs to these agents (empty -> all)
	Tasks     []string  // filter the pairs to these tasks (empty -> all)
	Suites    []string  // only expand these suites (empty -> all)
}

// newSandbox is sandbox.New, swappable in tests to run the orchestration
// against a fake sandbox (no docker).
var newSandbox = sandbox.New

// Run executes every suite (agent, task) pair and returns the run dir it wrote to.
// a single pair failing is logged and skipped -- it does not abort the run.
func Run(cfg *config.Config, opts Options) (string, error) {
	// absolute config dir: prepare/collect run with cwd=ConfigDir, and the
	// PATS_*_DIR paths must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	opts.Color = useColor(opts.Out) // decided once from the real terminal; po copies inherit it
	lg := logw{opts.Out, opts.Color}
	unlock, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return "", err
	}
	defer unlock()
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
	lg.info("%d pair(s) to run", len(pairs))
	// cancel on ctrl+C so the in-flight container is torn down, not orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// resolve images first -- everything after (preflight included) runs on
	// them: the default pats sandbox for sandboxes naming neither image nor
	// build, then build: contexts to image ids.
	defaultImages(cfg, opts, pairs)
	if err := buildImages(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}
	// pre-pull the egress proxy image (if any) before preflight, which itself
	// starts the proxy -- else the pull stalls the first check silently.
	if err := pullEgressImages(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.ID })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.ID })

	// fail fast on bad/expired creds before spinning per-pair containers.
	lg.info("preflight: checking agent credentials")
	if err := Preflight(ctx, cfg, opts, pairs); err != nil {
		return "", err
	}

	runDir, err := nextRunDir(filepath.Join(opts.ConfigDir, runsSubdir), opts.Now)
	if err != nil {
		return "", err
	}
	lg.info("run dir: %s", relToCwd(runDir))

	jobs := resolveJobs(opts.Jobs, dockerNCPU)

	// on a tty, draw a sticky progress region (total bar + a live line per
	// in-flight pair); otherwise (pipe/ci/tee) print plain per-pair lines.
	var bar *progress
	if isProgressTTY(opts.Out) {
		labelW := 0
		for _, p := range pairs {
			if w := len(p.Agent) + len(" x ") + len(p.Task); w > labelW {
				labelW = w
			}
		}
		bar = newProgress(opts.Out, len(pairs), labelW)
		defer bar.close()
	} else if jobs > 1 {
		lg.info("running %d pairs, up to %d in parallel", len(pairs), jobs)
	}

	// bounded worker pool. a failing pair is logged + skipped, never aborts
	// siblings -- so a plain semaphore (not errgroup's cancel-on-error). when
	// buffered (bar active, or jobs>1) each pair writes to its own buffer,
	// emitted atomically on completion so concurrent progress + host-script
	// output don't interleave; serial non-tty writes straight to opts.Out.
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	for _, p := range pairs {
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
		go func(p config.TestPair) {
			defer wg.Done()
			defer func() { <-sem }()

			label := p.Agent + " x " + p.Task
			po := opts
			var buf bytes.Buffer
			if bar != nil || jobs > 1 {
				po.Out = &buf
			}
			// always track per-pair counters: the bar shows them live, and the
			// completion line logs the final out/tool/net regardless of tty.
			stat := &pairStat{}
			if bar != nil {
				egressPath := filepath.Join(runDir, p.Agent, p.Task, "egress.log")
				bar.start(label, stat, egressPath, statScanner(agents[p.Agent].Harness) != nil)
			}
			if err := runPair(ctx, cfg, po, runDir, p, agents[p.Agent], tasks[p.Task], sandboxes, stat); err != nil {
				logw{po.Out, opts.Color}.error("[%s x %s] %v", p.Agent, p.Task, err)
			}
			switch {
			case bar != nil:
				bar.finish(label, buf.String())
			case jobs > 1:
				outMu.Lock()
				opts.Out.Write(buf.Bytes())
				outMu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return runDir, nil
}

// resolveJobs maps a Jobs option to a concrete worker count.
//
//	>0  -> that many
//	0   -> 1 (serial; the zero-value default)
//	<0  -> auto: autoN() (vm-accurate docker cpu count on macos for the run
//	       phase; host cpus for the score phase, which never touches docker),
//	       falling back to host cpus when autoN() reports < 1, clamped to
//	       [1,8]. pairs/cells are network-bound (model round-trips), so cpu is
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

// dockerNCPU returns the docker daemon's cpu count, or 0 if it can't be read.
// a var, like newSandbox, so a test can assert the score phase never reaches
// for it -- scoring runs no containers.
var dockerNCPU = func() int {
	out, err := exec.Command("docker", "info", "--format", "{{.NCPU}}").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

type pairMeta struct {
	Agent       string  `json:"agent"`
	Task        string  `json:"task"`
	Sandbox     string  `json:"sandbox"`
	Image       string  `json:"image"`
	ExitCode    int     `json:"exit_code"`
	DurationS   float64 `json:"duration_s"`
	Status      string  `json:"status"` // ok | nonzero | error
	Error       string  `json:"error,omitempty"`
	PatsVersion string  `json:"pats_version"`
	// hosts the agent tried to reach but egress denied (proxy mode). a non-empty
	// list flags attempted cheating / unexpected fetches.
	DeniedEgress []string `json:"denied_egress,omitempty"`
	// per-harness digest parsed from the log: cost, tokens, turns, tools.
	Summary *runSummary `json:"summary,omitempty"`
}

// deniedEgress reads the proxy audit log (one json line per request) and
// returns the unique denied targets -- the full url for mitm'd requests, the
// host otherwise. missing file -> nil (not proxy mode).
func deniedEgress(auditPath string) []string {
	data, err := os.ReadFile(auditPath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e struct {
			Host    string `json:"host"`
			URL     string `json:"url"`
			Allowed bool   `json:"allowed"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Allowed {
			continue
		}
		target := e.Host
		if e.URL != "" {
			target = e.URL
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

// runPair prepares, runs, and collects one (agent, task) pair. stat must be
// non-nil (every caller passes a fresh &pairStat{}) -- it's dereferenced
// unconditionally below, both to tap live counters and to record the final
// net count.
func runPair(
	ctx context.Context, cfg *config.Config, opts Options, runDir string, p config.TestPair,
	a config.Agent, t config.Task, sandboxes map[string]config.Sandbox, stat *pairStat,
) error {
	outDir := filepath.Join(runDir, p.Agent, p.Task)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	workDir, err := sandbox.MkTemp("pats-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir) // safe: agent runs as host uid, files are host-owned

	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return err
	}
	sb := sandboxes[sbID]

	// host-side env for the task's scripts (prepare, prompt-cmd, collect). they
	// run in the task's directory and seed/gather the sandbox via these paths;
	// the task's own env goes on top (Validate keeps it off PATS_*).
	taskDir := filepath.Join(opts.ConfigDir, t.Dir)
	hostEnv := map[string]string{
		"PATS_TASK_DIR":      taskDir,
		"PATS_RUN_ID":        filepath.Base(runDir),
		"PATS_RUN_DIR":       runDir,
		"PATS_AGENT_ID":      p.Agent,
		"PATS_AGENT_HARNESS": a.Harness,
		"PATS_TASK_ID":       p.Task,
		"PATS_MODEL":         a.Model,
		"PATS_WORKDIR":       workDir,
		"PATS_OUTPUT_DIR":    outDir,
	}

	// every agent is a harness: give it a writable HOME + wire creds. this
	// precedes prepare so prepare can seed harness config into it.
	h, err := newHarness(opts, a, sb, workDir, filepath.Base(runDir), filepath.Join(outDir, "egress.log"), p.Agent+" x "+p.Task)
	if err != nil {
		return err
	}
	defer h.cleanup()
	hostEnv["PATS_AGENT_HOME"] = h.homeDir
	for k, v := range t.Env {
		hostEnv[k] = v
	}

	if prepare := (step{t.Prepare, t.PrepareCmd, taskDir}); prepare.set() {
		if err := prepare.run(opts.ConfigDir, hostEnv, opts.Out); err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
	}

	promptData, err := taskPrompt(opts.ConfigDir, taskDir, t, hostEnv)
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	if err := os.WriteFile(h.promptPath(), promptData, 0o644); err != nil {
		return err
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
	r := h.run(ctx, stdoutW, stderrF, t.TimeoutDuration())
	status, errStr := "ok", ""
	switch {
	case r.timedOut:
		status, errStr = "timeout", fmt.Sprintf("timed out after %s", t.TimeoutDuration())
	case r.err != nil:
		status, errStr = "error", r.err.Error()
	case r.code != 0:
		status = "nonzero"
	}

	lg := logw{opts.Out, opts.Color}

	// collect when the agent ran and left something to gather: either it
	// finished (r.err == nil) or it timed out mid-run -- either way the
	// container produced files. a launch failure (r.err != nil, !timedOut)
	// left nothing to collect.
	if collect := (step{t.Collect, t.CollectCmd, taskDir}); (r.err == nil || r.timedOut) && collect.set() {
		if err := collect.run(opts.ConfigDir, hostEnv, opts.Out); err != nil {
			lg.warn("[%s x %s] collect: %v", p.Agent, p.Task, err)
		}
	}

	denied := deniedEgress(filepath.Join(outDir, "egress.log"))
	if len(denied) > 0 {
		lg.warn("[%s x %s] egress denied: %s", p.Agent, p.Task, strings.Join(denied, ", "))
	}

	meta := pairMeta{
		Agent: p.Agent, Task: p.Task,
		Sandbox: sbID, Image: sb.Image,
		ExitCode: r.code, DurationS: round2(r.dur), Status: status, Error: errStr,
		PatsVersion: version.Info.Version, DeniedEgress: denied,
		Summary: summarize(a.Harness, filepath.Join(outDir, "stdout.log")),
	}
	if err := writeJSON(filepath.Join(outDir, "metadata.json"), meta); err != nil {
		return err
	}

	// final net count straight from the audit log (the bar's poll may lag).
	if n, err := countLines(filepath.Join(outDir, "egress.log")); err == nil {
		atomic.StoreInt64(&stat.net, int64(n))
		atomic.StoreInt32(&stat.netSeen, 1)
	}
	// line 1: the verdict (level by status). line 2: the stats, indented under it.
	statusLine := fmt.Sprintf("[%s x %s] %s (exit %d, %.1fs)", p.Agent, p.Task, status, r.code, r.dur)
	switch status {
	case "ok":
		lg.info("%s", statusLine)
	case "nonzero", "timeout":
		lg.warn("%s", statusLine)
	default:
		lg.error("%s", statusLine)
	}
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
	lg.info("[%s x %s] %s", p.Agent, p.Task, stats)
	return nil
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
// label ("agent x task") prefixes the warning.
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
			os.RemoveAll(homeDir)
			return harnessSetup{}, err
		}
		if err := copyFile(cf, dst); err != nil {
			os.RemoveAll(homeDir)
			return harnessSetup{}, err
		}
		hasCreds = true
	}
	if !hasCreds {
		logw{opts.Out, opts.Color}.warn("[%s] no creds forwarded (expected %s); the harness may fail on auth", label, agent.CredentialHint(harness))
	}
	return harnessSetup{
		dir:     homeDir,
		mounts:  []sandbox.Mount{{Host: homeDir, Container: homeMount}},
		cleanup: func() { os.RemoveAll(homeDir) },
	}, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// egressFor renders a sandbox's egress policy for one agent run. under an
// allowlisting proxy the harness's own hosts (inference, token refresh) are
// merged in, so pats.yaml only lists what the task needs.
func egressFor(sb config.Sandbox, harness, auditPath string) sandbox.Egress {
	eg := sandbox.Egress{
		Mode:      sb.Egress.Mode,
		Default:   sb.Egress.Default,
		Allow:     sb.Egress.Allow,
		Deny:      sb.Egress.Deny,
		DenyURLs:  sb.Egress.DenyURLs,
		AllowURLs: sb.Egress.AllowURLs,
		Image:     sb.Egress.Image,
		AuditPath: auditPath,
	}
	if (eg.Mode == "proxy" || eg.Mode == "mitm-proxy") && eg.Default != "allow" {
		eg.Allow = mergeHosts(eg.Allow, agent.RequiredHosts[harness])
	}
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
// prompt-cmd, run after prepare with the task's env.
func taskPrompt(root, dir string, t config.Task, env map[string]string) ([]byte, error) {
	switch {
	case t.PromptFile != "":
		p := t.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return os.ReadFile(p)
	case t.PromptCmd != "":
		return step{cmd: t.PromptCmd, dir: dir}.output(root, env)
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

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
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
