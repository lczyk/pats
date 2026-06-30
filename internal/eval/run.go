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
}

// Run executes every test-matrix pair and returns the run dir it wrote to.
// a single pair failing is logged and skipped -- it does not abort the run.
func Run(cfg *config.Config, opts Options) (string, error) {
	// absolute config dir: prepare/collect run with cwd=ConfigDir, and the
	// PATS_*_DIR paths must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	unlock, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return "", err
	}
	defer unlock()
	pairs, err := cfg.ExpandTestMatrix()
	if err != nil {
		return "", err
	}
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.ID })
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.ID })

	// cancel on ctrl+C so the in-flight container is torn down, not orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// fail fast on bad/expired creds before spinning per-pair containers.
	if err := Preflight(ctx, cfg, opts); err != nil {
		return "", err
	}

	runDir, err := nextRunDir(filepath.Join(opts.ConfigDir, runsSubdir), opts.Now)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(opts.Out, "run dir: %s\n", runDir)

	jobs := resolveJobs(opts.Jobs)
	if jobs > 1 {
		fmt.Fprintf(opts.Out, "running %d pairs, up to %d in parallel\n", len(pairs), jobs)
	}

	// bounded worker pool. a failing pair is logged + skipped, never aborts
	// siblings -- so a plain semaphore (not errgroup's cancel-on-error). when
	// jobs>1 each pair writes to its own buffer, flushed atomically on completion
	// so concurrent progress + host-script output don't interleave; at jobs==1 we
	// write straight to opts.Out to keep host-script output live-streaming.
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	for _, p := range pairs {
		if ctx.Err() != nil {
			fmt.Fprintf(opts.Out, "interrupted -- stopping\n")
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p config.TestPair) {
			defer wg.Done()
			defer func() { <-sem }()

			po := opts
			var buf bytes.Buffer
			if jobs > 1 {
				po.Out = &buf
			}
			if err := runPair(ctx, cfg, po, runDir, p, agents[p.Agent], tasks[p.Task], sandboxes); err != nil {
				fmt.Fprintf(po.Out, "  [%s x %s] ERROR: %v\n", p.Agent, p.Task, err)
			}
			if jobs > 1 {
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
	Weight      float64 `json:"weight"`
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
}

// deniedEgress reads the proxy audit log (one json line per request) and
// returns the unique hosts that were denied. missing file -> nil (not proxy mode).
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
			Allowed bool   `json:"allowed"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Allowed || seen[e.Host] {
			continue
		}
		seen[e.Host] = true
		out = append(out, e.Host)
	}
	return out
}

func runPair(
	ctx context.Context, cfg *config.Config, opts Options, runDir string, p config.TestPair,
	a config.Agent, t config.Task, sandboxes map[string]config.Sandbox,
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
		"PATS_AGENT":      p.Agent,
		"PATS_TASK":       p.Task,
		"PATS_MODEL":      a.Model,
		"PATS_WORKDIR":    workDir,
		"PATS_OUTPUT_DIR": outDir,
	}

	if t.Prepare != "" {
		if err := runHost(opts, t.Prepare, hostEnv); err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
	}

	// stage the prompt into the workdir so the agent sees it at a stable path.
	promptData, err := os.ReadFile(filepath.Join(opts.ConfigDir, t.PromptFile))
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
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
	spec.Egress = sandbox.Egress{
		Mode:      sb.Egress.Mode,
		Default:   sb.Egress.Default,
		Allow:     sb.Egress.Allow,
		Deny:      sb.Egress.Deny,
		Image:     sb.Egress.Image,
		AuditPath: filepath.Join(outDir, "egress.log"),
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

	// agent output goes to the log files only -- keep the run terminal to pats'
	// own progress lines. (stream-json still lands in stdout.log for later.)
	t0 := time.Now()
	code, runErr := box.Run(ctx, spec, stdoutF, stderrF)
	dur := time.Since(t0).Seconds()

	status, errStr := "ok", ""
	switch {
	case runErr != nil:
		status, errStr = "error", runErr.Error()
	case code != 0:
		status = "nonzero"
	}

	// collect only if the agent ran (a launch failure left nothing to gather).
	if runErr == nil && t.Collect != "" {
		if err := runHost(opts, t.Collect, hostEnv); err != nil {
			fmt.Fprintf(opts.Out, "  [%s x %s] collect: %v\n", p.Agent, p.Task, err)
		}
	}

	denied := deniedEgress(filepath.Join(outDir, "egress.log"))
	if len(denied) > 0 {
		fmt.Fprintf(opts.Out, "  [%s x %s] egress denied: %s\n", p.Agent, p.Task, strings.Join(denied, ", "))
	}

	meta := pairMeta{
		Agent: p.Agent, Task: p.Task, Weight: p.Weight,
		Sandbox: sbID, Image: sb.Image,
		ExitCode: code, DurationS: round2(dur), Status: status, Error: errStr,
		PatsVersion: version.Version, DeniedEgress: denied,
	}
	if err := writeJSON(filepath.Join(outDir, "metadata.json"), meta); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "  [%s x %s] %s (exit %d, %.1fs)\n", p.Agent, p.Task, status, code, dur)
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
		fmt.Fprintf(opts.Out, "  [%s x %s] WARNING: no creds forwarded (no token env, no ~/.claude/.credentials.json); the harness may fail on auth\n", p.Agent, p.Task)
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

// runHost runs a prepare/collect command on the host, in the project dir, with
// the PATS_* env appended. output is streamed to opts.Out.
func runHost(opts Options, command string, env map[string]string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = opts.ConfigDir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = opts.Out
	cmd.Stderr = opts.Out
	return cmd.Run()
}

// nextRunDir creates and returns base/<yyyymmdd>-<nnn>, n = highest existing + 1, zero-padded to width 3.
func nextRunDir(base string, now time.Time) (string, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	date := now.UTC().Format("20060102")
	prefix := date + "-"
	max := 0
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), prefix)); err == nil && strings.HasPrefix(e.Name(), prefix) && n > max {
			max = n
		}
	}
	dir := filepath.Join(base, fmt.Sprintf("%s-%03d", date, max+1))
	return dir, os.MkdirAll(dir, 0o755)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func index[T any](xs []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(xs))
	for _, x := range xs {
		m[key(x)] = x
	}
	return m
}
