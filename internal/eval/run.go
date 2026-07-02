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
	"github.com/lczyk/pats/internal/sandbox"
	"github.com/lczyk/pats/internal/version"
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
	if pairs, err = config.FilterPairs(pairs, opts.Agents, opts.Tasks); err != nil {
		return "", err
	}
	// cancel on ctrl+C so the in-flight container is torn down, not orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// resolve build: sandboxes to image ids first -- everything after (preflight
	// included) then runs on the built image.
	if err := buildImages(ctx, cfg, opts, pairs); err != nil {
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

	jobs := resolveJobs(opts.Jobs)

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
				bar.start(label, stat, egressPath, statScanner(agents[p.Agent].Kind) != nil)
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

// resolveJobs maps the Jobs option to a concrete worker count.
//
//	>0  -> that many
//	0   -> 1 (serial; the zero-value default)
//	<0  -> auto: docker's own cpu count (vm-accurate on macos), falling back to
//	       host cpus, clamped to [1,8]. pairs are network-bound (model
//	       round-trips), so cpu is a rough proxy, not a real limit -- the clamp
//	       stops a big box from hammering the daemon + tripping rate-limits.
func resolveJobs(j int) int {
	if j > 0 {
		return j
	}
	if j == 0 {
		return 1
	}
	n := dockerNCPU()
	if n < 1 {
		n = runtime.NumCPU()
	}
	return min(max(n, 1), 8)
}

// dockerNCPU returns the docker daemon's cpu count, or 0 if it can't be read.
func dockerNCPU() int {
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
	// per-kind digest parsed from the harness log: cost, tokens, turns, tools.
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

func runPair(
	ctx context.Context, cfg *config.Config, opts Options, runDir string, p config.TestPair,
	a config.Agent, t config.Task, sandboxes map[string]config.Sandbox, stat *pairStat,
) error {
	outDir := filepath.Join(runDir, p.Agent, p.Task)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	workDir, err := os.MkdirTemp("", "pats-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir) // safe: agent runs as host uid, files are host-owned

	sbID, err := cfg.ResolveSandbox(a)
	if err != nil {
		return err
	}
	sb := sandboxes[sbID]
	box, err := sandbox.New(sb.ResolvedDriver(), sb.Image)
	if err != nil {
		return err
	}

	// host-side env for prepare/collect scripts (they run in the project dir
	// and seed/gather the sandbox via these paths).
	hostEnv := map[string]string{
		"PATS_AGENT_ID":   p.Agent,
		"PATS_AGENT_KIND": a.Kind,
		"PATS_TASK_ID":    p.Task,
		"PATS_MODEL":      a.Model,
		"PATS_WORKDIR":    workDir,
		"PATS_OUTPUT_DIR": outDir,
	}

	if t.Prepare != "" {
		if err := runHost(opts, expandID(t.Prepare, t.ID), hostEnv); err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
	}

	// stage the prompt into the workdir so the agent sees it at a stable path.
	promptData, err := resolvePrompt(opts.ConfigDir, expandID(t.Prompt, t.ID), hostEnv)
	if err != nil {
		return fmt.Errorf("resolve prompt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "prompt.txt"), promptData, 0o644); err != nil {
		return err
	}

	env := agent.Env(a, sandbox.WorkMount+"/prompt.txt", sandbox.WorkMount)
	cenv, hasToken := agent.CredEnv()
	maps.Copy(env, cenv) // forward host creds to any task-running agent

	// every agent is a harness: give it a writable HOME + wire creds.
	hs, herr := harnessHome(opts, p, env, hasToken)
	if herr != nil {
		return herr
	}
	defer hs.cleanup()

	spec, err := agent.Spec(a, workDir, env)
	if err != nil {
		return err
	}
	spec.Mounts = hs.mounts
	spec.Egress = egressFor(sb, a.Kind, filepath.Join(outDir, "egress.log"))

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

	// agent output goes to the log file. when a progress bar is active we also
	// tap it through statTap for the in-run counters (out lines + per-kind tool
	// parse) -- the full stdout.log is still written intact.
	var stdoutW io.Writer = stdoutF
	if stat != nil {
		stdoutW = io.MultiWriter(stdoutF, &statTap{stat: stat, scan: statScanner(a.Kind)})
	}
	// bound the agent run with the configured timeout (0 = none) so a stuck pair
	// gets killed rather than hanging the whole run. ctx cancel tears down the
	// container (same path as ctrl+C).
	rctx := ctx
	if to := t.TimeoutDuration(); to > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	t0 := time.Now()
	code, runErr := box.Run(rctx, spec, stdoutW, stderrF)
	dur := time.Since(t0).Seconds()

	status, errStr := "ok", ""
	switch {
	case runErr != nil && errors.Is(rctx.Err(), context.DeadlineExceeded):
		status, errStr = "timeout", fmt.Sprintf("timed out after %s", t.TimeoutDuration())
	case runErr != nil:
		status, errStr = "error", runErr.Error()
	case code != 0:
		status = "nonzero"
	}

	lg := logw{opts.Out, opts.Color}

	// collect only if the agent ran (a launch failure left nothing to gather).
	if runErr == nil && t.Collect != "" {
		if err := runHost(opts, expandID(t.Collect, t.ID), hostEnv); err != nil {
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
		ExitCode: code, DurationS: round2(dur), Status: status, Error: errStr,
		PatsVersion: version.Info.Version, DeniedEgress: denied,
		Summary: summarize(a.Kind, filepath.Join(outDir, "stdout.log")),
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
	statusLine := fmt.Sprintf("[%s x %s] %s (exit %d, %.1fs)", p.Agent, p.Task, status, code, dur)
	switch status {
	case "ok":
		lg.info("%s", statusLine)
	case "nonzero", "timeout":
		lg.warn("%s", statusLine)
	default:
		lg.error("%s", statusLine)
	}
	stats := stat.cols(statScanner(a.Kind) != nil)
	if s := meta.Summary; s != nil {
		stats += fmt.Sprintf("  %s tok  $%.2f  %d turns", humanK(s.OutputTokens), s.CostUSD, s.NumTurns)
	}
	lg.info("[%s x %s] %s", p.Agent, p.Task, stats)
	return nil
}

// homeMount is the in-sandbox HOME for a harness -- a writable dir the agent
// (running as an arbitrary uid) owns, where its creds + cache live.
const homeMount = "/pats-home"

type harnessSetup struct {
	mounts  []sandbox.Mount
	cleanup func()
}

// harnessHome gives a harness a writable HOME (the --user uid owns nothing) and
// wires claude's oauth creds mason-style: mount ~/.claude/.credentials.json (rw,
// claude rotates it) into the home if present. token/key env is forwarded by the
// caller. warns when no creds are available at all, since the cli then fails auth.
func harnessHome(opts Options, p config.TestPair, env map[string]string, hasToken bool) (harnessSetup, error) {
	homeDir, err := os.MkdirTemp("", "pats-home-")
	if err != nil {
		return harnessSetup{}, err
	}
	env["HOME"] = homeMount

	hasCreds := hasToken
	if cf := agent.HostCredsFile(); cf != "" {
		dst := filepath.Join(homeDir, ".claude")
		if err := os.MkdirAll(dst, 0o700); err != nil {
			os.RemoveAll(homeDir)
			return harnessSetup{}, err
		}
		if err := copyFile(cf, filepath.Join(dst, ".credentials.json")); err != nil {
			os.RemoveAll(homeDir)
			return harnessSetup{}, err
		}
		hasCreds = true
	}
	if !hasCreds {
		logw{opts.Out, opts.Color}.warn("[%s x %s] no creds forwarded (no token env, no ~/.claude/.credentials.json); the harness may fail on auth", p.Agent, p.Task)
	}
	return harnessSetup{
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

// expandID substitutes ${id} in a run-field value with the owning entity's id.
func expandID(s, id string) string { return strings.ReplaceAll(s, "${id}", id) }

// egressFor renders a sandbox's egress policy for one agent run. under an
// allowlisting proxy the harness's own hosts (inference, token refresh) are
// merged in, so pats.yaml only lists what the task needs.
func egressFor(sb config.Sandbox, kind, auditPath string) sandbox.Egress {
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
		eg.Allow = mergeHosts(eg.Allow, agent.RequiredHosts[kind])
	}
	return eg
}

// mergeHosts appends extras not already in hosts (exact string match; overlap
// via wildcards is harmless -- the proxy just sees a redundant entry).
func mergeHosts(hosts, extras []string) []string {
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	for _, e := range extras {
		if !seen[e] {
			hosts = append(hosts, e)
		}
	}
	return hosts
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

// commandFor tokenizes a run-field value (POSIX-sh quoting, no shell/expansion)
// and returns an *exec.Cmd that runs argv[0] -- a +x regular file resolved
// against configDir -- with the remaining tokens as literal args, in configDir,
// with os env + the given PATS_* env. errors on bad quoting, an empty command,
// or argv[0] not being an executable file.
func commandFor(configDir, spec string, env map[string]string) (*exec.Cmd, error) {
	argv, err := splitArgs(spec)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	path := argv[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("%s is not executable (chmod +x)", path)
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = configDir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd, nil
}

// resolvePrompt turns a task/scorer prompt spec into the actual prompt bytes.
// it tokenises the spec (POSIX-sh quoting) and inspects argv[0]:
//
//	a +x regular file        -> exec argv (a generator), its stdout is the prompt
//	a non-executable file     -> the file's contents (no args allowed)
//	anything else / bad quote -> the whole spec, verbatim, is the literal prompt
//
// the last rule is what lets a free-text prompt ("write a readme", or one with
// an apostrophe) pass through untouched. env (PATS_*) is passed to a generator.
func resolvePrompt(configDir, spec string, env map[string]string) ([]byte, error) {
	argv, err := splitArgs(spec)
	if err != nil || len(argv) == 0 {
		return []byte(spec), nil // unparseable quoting -> literal
	}
	path := argv[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	fi, serr := os.Stat(path)
	if serr != nil || !fi.Mode().IsRegular() {
		return []byte(spec), nil // not a file -> literal prompt
	}
	if fi.Mode()&0o111 == 0 {
		// a plain file's contents are the prompt; trailing args are meaningless
		// here -- erroring beats silently dropping them (and a literal prompt
		// whose first word collides with a file would otherwise vanish).
		if len(argv) > 1 {
			return nil, fmt.Errorf("prompt %q is not executable but has args (chmod +x to run it, or drop the args)", argv[0])
		}
		return os.ReadFile(path) // plain file -> contents
	}
	cmd, err := commandFor(configDir, spec, env) // executable -> stdout
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("exec prompt %s: %w%s", argv[0], err, stderrTail(&errb))
	}
	return out.Bytes(), nil
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

// runHost runs a prepare/collect field on the host: it execs the field's file
// (with args) in the project dir, PATS_* env appended, no shell. output streams
// to opts.Out. callers expand ${id} first.
func runHost(opts Options, command string, env map[string]string) error {
	cmd, err := commandFor(opts.ConfigDir, command, env)
	if err != nil {
		return err
	}
	cmd.Stdout = opts.Out
	cmd.Stderr = opts.Out
	return cmd.Run()
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
