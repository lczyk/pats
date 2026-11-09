package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

func dockerOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not responding")
	}
}

// full run-phase e2e: a harness agent runs in a sandbox, reads the staged
// prompt, writes a result; the collect script copies it to the output dir;
// metadata records ok. proves prepare/agent/collect + run-dir layout. the
// harness's command is overridden with a no-cred stand-in.
func TestRunE2E(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("do the thing"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "collect.sh"),
		[]byte("#!/bin/sh\ncp \"$PATS_WORKDIR/result.txt\" \"$PATS_OUTPUT_DIR/\"\n"), 0o755))

	old := agent.HarnessCmds["opencode-openrouter"]
	agent.HarnessCmds["opencode-openrouter"] = `cat "$PATS_PROMPT_FILE" > result.txt; echo "model=$PATS_MODEL"`
	defer func() { agent.HarnessCmds["opencode-openrouter"] = old }()

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: ubuntu:26.04}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m1, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, collect: ./collect.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	// every run after init builds and pulls nothing, here or in the driver.
	calls := dockerSpy(t)
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, "%s", out.String())
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assertNoBuildOrPull(t, calls)
	require.NoError(t, err)

	outDir := filepath.Join(runDir, "a", "t")

	stdout, err := os.ReadFile(filepath.Join(outDir, "stdout.log"))
	require.NoError(t, err)
	assert.ContainsString(t, string(stdout), "model=m1")

	// collect copied the agent's result (which echoed the staged prompt).
	result, err := os.ReadFile(filepath.Join(outDir, "result.txt"))
	require.NoError(t, err)
	assert.ContainsString(t, string(result), "do the thing")

	meta, err := os.ReadFile(filepath.Join(outDir, "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "ok"`)
}

func TestNextRunDirIncrements(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	d1, err := nextRunDir(base, now)
	require.NoError(t, err)
	assert.ContainsString(t, filepath.Base(d1), "001-20260621-")
	// words are deterministic from the numeric prefix.
	assert.Equal(t, filepath.Base(d1), "001-20260621-"+generateName("001-20260621"))
	d2, err := nextRunDir(base, now)
	require.NoError(t, err)
	assert.ContainsString(t, filepath.Base(d2), "002-20260621-")

	// "latest" symlink tracks the newest run dir.
	target, err := os.Readlink(filepath.Join(base, "latest"))
	require.NoError(t, err)
	assert.Equal(t, target, filepath.Base(d2))
}

func TestNextRunDirCountsAcrossDates(t *testing.T) {
	// the counter is global, not per-date.
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "007-20260620-woven-sock"), 0o755))
	d, err := nextRunDir(base, time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.ContainsString(t, filepath.Base(d), "008-20260621-")
}

func TestResolveJobs(t *testing.T) {
	panicN := func() int { panic("autoN must not be called when j >= 0") }
	assert.Equal(t, resolveJobs(4, panicN), 4) // explicit count passes through, autoN untouched
	assert.Equal(t, resolveJobs(1, panicN), 1) // serial
	assert.Equal(t, resolveJobs(0, panicN), 1) // zero-value -> serial, autoN untouched

	// auto (<0): autoN() clamped to [1,8].
	cases := map[string]struct {
		autoN int
		want  int
	}{
		"mid-range passes through": {autoN: 4, want: 4},
		"clamped to the ceiling":   {autoN: 20, want: 8},
		"floor of one":             {autoN: 1, want: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, resolveJobs(-1, func() int { return c.autoN }), c.want)
		})
	}

	// autoN reporting < 1 (e.g. docker unreachable) falls back to runtime.NumCPU.
	for _, n := range []int{0, -3} {
		got := resolveJobs(-1, func() int { return n })
		assert.That(t, got >= 1 && got <= 8, "fallback jobs in [1,8], got", got)
	}
}

func TestTaskPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cases", "a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.md"), []byte("from file\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gen.sh"),
		[]byte("#!/bin/sh\necho \"gen $PATS_TASK_ID $1 $(basename \"$PWD\")\"\n"), 0o755))

	// literal text, verbatim -- quotes, apostrophes and ${...} included.
	got, err := taskPrompt(root, dir, config.Task{Prompt: "it's ${not} a template"}, nil)
	require.NoError(t, err)
	assert.Equal(t, string(got), "it's ${not} a template")

	// prompt-file resolves against the task's dir.
	got, err = taskPrompt(root, dir, config.Task{PromptFile: "p.md"}, nil)
	require.NoError(t, err)
	assert.Equal(t, string(got), "from file\n")

	// prompt-cmd: stdout, run in the task's dir with env + args passed through.
	got, err = taskPrompt(root, dir, config.Task{PromptCmd: "./gen.sh --flavour"}, map[string]string{"PATS_TASK_ID": "t1"})
	require.NoError(t, err)
	assert.Equal(t, string(got), "gen t1 --flavour a\n")

	// a missing prompt-file is an error, never a literal fallback.
	_, err = taskPrompt(root, dir, config.Task{PromptFile: "nope.md"}, nil)
	assert.Error(t, err, assert.AnyError)
}

func TestHumanK(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1.0k", 12345: "12.3k"}
	for n, want := range cases {
		if got := humanK(n); got != want {
			t.Errorf("humanK(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestMergeHosts(t *testing.T) {
	got := mergeHosts([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("mergeHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeHosts = %v, want %v", got, want)
		}
	}
}

// COVER: mergeHosts must not append into the caller's slice -- that slice is
// cfg.Sandboxes[i].Egress.Allow, shared by every concurrent pair, so an
// in-place append would let one pair's harness hosts land in another's
// allowlist. spare capacity is what makes append reuse the backing array, so
// the test has to create some: a plain literal (cap == len, which is also what
// yaml decoding produces) always reallocates and would pass either way.
func TestMergeHostsDoesNotAliasCaller(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "base.example.com"

	first := mergeHosts(shared, []string{"a.example.com"})
	second := mergeHosts(shared, []string{"b.example.com"})

	assert.Equal(t, first[1], "a.example.com") // clobbered by the second call pre-fix
	assert.Equal(t, second[1], "b.example.com")
	assert.Equal(t, len(shared), 1) // and the caller's own view is untouched
}

func TestStderrTail(t *testing.T) {
	if got := stderrTail(bytes.NewBufferString("  \n ")); got != "" {
		t.Errorf("empty buffer: got %q, want \"\"", got)
	}
	if got := stderrTail(bytes.NewBufferString("boom")); got != "\nstderr: boom" {
		t.Errorf("short buffer: got %q", got)
	}
	var many []string
	for i := range 15 {
		many = append(many, strconv.Itoa(i))
	}
	got := stderrTail(bytes.NewBufferString(strings.Join(many, "\n")))
	if !strings.HasPrefix(got, "\nstderr: ... ") || !strings.HasSuffix(got, "14") {
		t.Errorf("long buffer not truncated to last 10 lines: got %q", got)
	}
}

// fakeSandbox lets Run's orchestration execute without docker: it records the
// spec and plays back a canned exit code / output per agent id.
type fakeSandbox struct {
	code   int
	stdout string
	err    error
	calls  int
	// blockUntilDone waits for the run's ctx to be cancelled (the task's
	// timeout firing) before returning code/err -- simulates a task that's
	// still running when the deadline hits, real docker's SIGTERM-then-exit.
	blockUntilDone bool
	// onRun, if set, sees the pair run's spec while its mounts still exist.
	onRun func(spec sandbox.Spec)
	// onClose, if set, sees the pair's spec as its session closes.
	onClose func(spec sandbox.Spec)
	// startErr fails the session start; onRoot answers root execs.
	startErr error
	onRoot   func(cmd sandbox.Cmd, stdout io.Writer) (int, error)
	rootCmds []sandbox.Cmd
	pushed   []string
}

func (f *fakeSandbox) Run(ctx context.Context, spec sandbox.Spec, stdout, stderr io.Writer) (int, error) {
	f.calls++
	return 0, nil
}

// Start gives the credential preflight (its workdir says so) a session that
// always passes, so the canned outcome lands on the pair's agent exec.
func (f *fakeSandbox) Start(ctx context.Context, spec sandbox.Spec) (sandbox.Session, error) {
	preflight := strings.HasPrefix(filepath.Base(spec.Workdir), "pats-preflight-")
	if f.startErr != nil && !preflight {
		return nil, f.startErr
	}
	return &fakeSession{f: f, spec: spec, preflight: preflight}, nil
}

// fakeSession runs root commands (prepare, its cleanup, the hand-over) as
// recorded no-ops, and the agent with the fake's canned outcome.
type fakeSession struct {
	f         *fakeSandbox
	spec      sandbox.Spec
	preflight bool
}

func (s *fakeSession) Push(ctx context.Context, hostPath, path string) error {
	s.f.pushed = append(s.f.pushed, path)
	return nil
}

func (s *fakeSession) Pull(ctx context.Context, path, hostDir string) error { return nil }

func (s *fakeSession) HandOver(ctx context.Context, paths ...string) error { return nil }

func (s *fakeSession) Sync(ctx context.Context) error { return nil }

func (s *fakeSession) Close() error {
	if s.f.onClose != nil && !s.preflight {
		s.f.onClose(s.spec)
	}
	return nil
}

func (s *fakeSession) Exec(ctx context.Context, cmd sandbox.Cmd, stdout, stderr io.Writer) (int, error) {
	f := s.f
	if cmd.Root {
		f.rootCmds = append(f.rootCmds, cmd)
		if f.onRoot != nil {
			return f.onRoot(cmd, stdout)
		}
		return 0, nil
	}
	f.calls++
	if s.preflight {
		return 0, nil
	}
	if f.onRun != nil {
		spec := s.spec
		spec.Argv, spec.Env = cmd.Argv, cmd.Env
		f.onRun(spec)
	}
	if f.blockUntilDone {
		<-ctx.Done()
	}
	if f.err != nil {
		return -1, f.err
	}
	io.WriteString(stdout, f.stdout)
	return f.code, nil
}

// COVER: Run's orchestration -- run-dir layout, per-pair metadata, status
// classification (ok vs nonzero vs error), failing pair not aborting siblings
// -- exercised against a fake sandbox, no docker needed.
func TestRunOrchestrationFakeSandbox(t *testing.T) {
	fakes := map[string]*fakeSandbox{
		"ok":   {code: 0, stdout: "fine\n"},
		"bad":  {code: 3},
		"boom": {err: errors.New("launch failed")},
	}
	old := newSandbox
	// the image name doubles as the fake selector: each agent gets its own
	// sandbox with image = fake key.
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fakes[s.Image], nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))

	src := "boxes: {kind: sandbox, items: {"
	for id := range fakes {
		src += id + ": {image: " + id + "}, "
	}
	src += "}}\nagents: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {"
	for id := range fakes {
		src += id + ": {sandbox: " + id + "}, "
	}
	src += "}}\nt: {kind: task, items: {t: {prompt-file: prompt.txt}}}\n"
	src += "su: {kind: suite, items: {su: {agents: agents, tasks: t}}}\n"
	cfg := parseCfg(t, src)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err) // a failing pair is logged, not fatal

	wantStatus := map[string]string{"ok": "ok", "bad": "nonzero", "boom": "error"}
	for id, want := range wantStatus {
		meta, err := os.ReadFile(filepath.Join(runDir, id, "t", "metadata.json"))
		require.NoError(t, err)
		assert.ContainsString(t, string(meta), `"status": "`+want+`"`)
	}
	stdout, err := os.ReadFile(filepath.Join(runDir, "ok", "t", "stdout.log"))
	require.NoError(t, err)
	assert.Equal(t, string(stdout), "fine\n")
}

// the proxy's audit tail lands only once the session closes (its log
// follower drains then): metadata is folded after that, so a denied host
// logged last is still in it.
func TestRunFoldsAuditAfterClose(t *testing.T) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		return &fakeSandbox{onClose: func(spec sandbox.Spec) {
			f, err := os.OpenFile(spec.Egress.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			defer f.Close()
			_, _ = f.WriteString(`{"ev":"open","host":"late.example","rule":"default"}` + "\n")
		}}, nil
	}
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	meta, err := os.ReadFile(filepath.Join(runDir, "a", "t", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"late.example"`)
	assert.ContainsString(t, out.String(), "egress denied (no rule): late.example")
}

// the default is the verdict per pair; -v adds the detail lines and the
// run's totals; -vv streams the host scripts, which log either way.
func TestRunVerbosity(t *testing.T) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		return &fakeSandbox{stdout: "fine\n"}, nil
	}
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, stage: echo staged, collect: echo collected}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	pinAll(t, dir, cfg)
	for v, c := range []struct{ has, hasnt []string }{
		{[]string{"INFO a/t ok (exit 0"}, []string{"run dir", "1 ok", "staged", "collected"}},
		{[]string{"INFO a/t ok (exit 0", "INFO run dir", "INFO 1 ok"}, []string{"staged", "collected"}},
		{[]string{"INFO a/t ok (exit 0", "INFO 1 ok", "staged", "collected"}, nil},
	} {
		var out bytes.Buffer
		runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Verbose: v})
		require.NoError(t, err)
		for _, w := range c.has {
			assert.ContainsString(t, out.String(), w, "-v x%d", v)
		}
		for _, w := range c.hasnt {
			assert.That(t, !strings.Contains(out.String(), w), "-v x%d printed %q:\n%s", v, w, out.String())
		}
		for name, want := range map[string]string{"stage.log": "staged\n", "collect.log": "collected\n"} {
			b, err := os.ReadFile(filepath.Join(runDir, "a", "t", name))
			require.NoError(t, err)
			assert.Equal(t, string(b), want)
		}
	}
}

// COVER: a deadline whose sandbox reports a plain non-zero exit with no
// error -- exactly what sandbox.container.Run does today, since it maps any
// *exec.ExitError (including the 143 from a SIGTERM'd container) to
// (code, nil). runErr alone can't distinguish that from an agent that just
// failed; the status has to come from rctx.Err() instead. fails against the
// old runPair, which only classified a pair as "timeout" when runErr != nil.
func TestRunPairTimeoutStatus(t *testing.T) {
	fake := &fakeSandbox{blockUntilDone: true, code: 143} // SIGTERM exit, no error -- the coin-flip case
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, timeout: 30ms}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)

	meta, err := os.ReadFile(filepath.Join(runDir, "a", "t", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "timeout"`)
	assert.ContainsString(t, string(meta), `"error": "timed out after`)
}

// COVER: collect must run on a timed-out pair too -- the agent DID run and
// left files behind, unlike a launch failure. uses a genuine runErr (not the
// nil-error SIGTERM case above, which the old code already classified as
// "timeout" by coincidence) so this actually exercises the
// runErr == nil || timedOut collect condition, and fails against the old
// runErr == nil gate.
func TestRunPairCollectsOnTimeout(t *testing.T) {
	var sbEnv map[string]string
	fake := &fakeSandbox{blockUntilDone: true, err: errors.New("some docker hiccup"),
		onRun: func(spec sandbox.Spec) { sbEnv = spec.Env }}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "collect.sh"),
		[]byte("#!/bin/sh\ntouch \"$PATS_OUTPUT_DIR/collected\"\nprintf '%s\\n%s\\n' \"$PATS_RUN_DIR\" \"$PATS_RUN_ID\" > \"$PATS_OUTPUT_DIR/run-env\"\n"), 0o755))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, timeout: 30ms, collect: ./collect.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)

	meta, err := os.ReadFile(filepath.Join(runDir, "a", "t", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "timeout"`)

	_, err = os.Stat(filepath.Join(runDir, "a", "t", "collected"))
	assert.NoError(t, err) // collect ran despite runErr != nil, because the pair timed out

	// host scripts see the run dir and its basename; the sandbox gets the id only.
	runEnv, err := os.ReadFile(filepath.Join(runDir, "a", "t", "run-env"))
	require.NoError(t, err)
	assert.Equal(t, string(runEnv), runDir+"\n"+filepath.Base(runDir)+"\n")
	assert.Equal(t, sbEnv["PATS_RUN_ID"], filepath.Base(runDir))
	assert.Equal(t, sbEnv["PATS_RUN_DIR"], "")
}

// COVER: the other half of the collect gate -- sandbox setup (egress in
// particular) runs under the same deadline as the agent, so a setup failure can
// arrive with rctx already expired. that's a launch failure with an empty
// workdir, not a timed-out agent: the status must name the real cause and
// collect must not run. fails against a timedOut check that looks only at
// rctx.Err().
func TestRunPairLaunchFailureUnderDeadline(t *testing.T) {
	fake := &fakeSandbox{
		blockUntilDone: true,
		err:            fmt.Errorf("%w: egress: create network: boom", sandbox.ErrLaunch),
	}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "collect.sh"),
		[]byte("#!/bin/sh\ntouch \"$PATS_OUTPUT_DIR/collected\"\n"), 0o755))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, timeout: 30ms, collect: ./collect.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)

	meta, err := os.ReadFile(filepath.Join(runDir, "a", "t", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"status": "error"`)
	assert.ContainsString(t, string(meta), "create network") // the real cause, not "timed out after"

	_, err = os.Stat(filepath.Join(runDir, "a", "t", "collected"))
	assert.That(t, os.IsNotExist(err), "collect must not run when the sandbox never launched")
}

func homeMountHost(spec sandbox.Spec) string {
	for _, m := range spec.Mounts {
		if m.Container == homeMount {
			return m.Host
		}
	}
	return ""
}

// prepare seeds harness config into the sandbox HOME via PATS_AGENT_HOME, the agent
// sees it, collect can read back what the agent left there, and the HOME is
// gone once the pair is done.
func TestRunPairPrepareSeedsHome(t *testing.T) {
	var home string
	var seeded []byte
	fake := &fakeSandbox{onRun: func(spec sandbox.Spec) {
		home = homeMountHost(spec)
		seeded, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		_ = os.WriteFile(filepath.Join(home, "agent-wrote"), []byte("x"), 0o644)
	}}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prepare.sh"),
		[]byte("#!/bin/sh\nmkdir -p \"$PATS_AGENT_HOME/.claude\" && echo seeded > \"$PATS_AGENT_HOME/.claude/settings.json\"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "collect.sh"),
		[]byte("#!/bin/sh\ncp \"$PATS_AGENT_HOME/agent-wrote\" \"$PATS_OUTPUT_DIR/\"\n"), 0o755))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, stage: ./prepare.sh, collect: ./collect.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)

	require.That(t, home != "", "pair run has no HOME mount")
	assert.Equal(t, string(seeded), "seeded\n")
	_, err = os.Stat(filepath.Join(runDir, "a", "t", "agent-wrote"))
	assert.NoError(t, err)
	_, err = os.Stat(home)
	assert.That(t, os.IsNotExist(err), "HOME not cleaned up after the pair")
}

// a sandbox naming neither image nor build runs on the published pats
// sandbox pinned to this version.
func TestRunDefaultSandboxImage(t *testing.T) {
	var image string
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		image = s.Image
		return &fakeSandbox{}, nil
	}
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	h := newFakeHost()
	useHost(t, h)

	// before init the run stops, naming the command.
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assert.Error(t, err, "run `pats init` first")

	// init pulls the published sandbox, and the run starts from its id.
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}))
	want, _ := sandbox.SandboxImage("")
	assert.EqualArrays(t, h.pulls, []string{want})
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	assert.Equal(t, image, "sha256:pulled-"+want)

	// runs after init build and pull nothing.
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	assert.EqualArrays(t, h.pulls, []string{want})
}

// the prompt is staged in the harness HOME, never in the workdir the agent
// sees as its cwd.
func TestRunPairPromptOutsideWorkdir(t *testing.T) {
	var promptFile, staged string
	var inWorkdir bool
	fake := &fakeSandbox{onRun: func(spec sandbox.Spec) {
		promptFile = spec.Env["PATS_PROMPT_FILE"]
		data, _ := os.ReadFile(filepath.Join(homeMountHost(spec), promptName))
		staged = string(data)
		_, err := os.Stat(filepath.Join(spec.Workdir, promptName))
		inWorkdir = err == nil
	}}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("do the thing"), 0o644))
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)

	assert.Equal(t, promptFile, homeMount+"/"+promptName)
	assert.Equal(t, staged, "do the thing")
	assert.That(t, !inWorkdir, "prompt staged in the workdir")
}

// a failing stage still tears down the HOME it was handed.
func TestRunPairStageFailureCleansHome(t *testing.T) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return &fakeSandbox{}, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prepare.sh"),
		[]byte("#!/bin/sh\nprintf %s \"$PATS_AGENT_HOME\" > \"$PATS_OUTPUT_DIR/home-path\"\nexit 1\n"), 0o755))

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt-file: prompt.txt, stage: ./prepare.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, _ := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})

	home, err := os.ReadFile(filepath.Join(runDir, "a", "t", "home-path"))
	require.NoError(t, err)
	require.That(t, len(home) > 0, "prepare saw an empty PATS_AGENT_HOME")
	_, err = os.Stat(string(home))
	assert.That(t, os.IsNotExist(err), "HOME not cleaned up after a failed prepare")
}

// COVER: a config with no suites is deliberately allowed by Validate -- see
// its "nothing to orphan-check against" early return -- so it expands to zero
// pairs. Run must say that, not fall through to FilterPairs and blame an
// --agent/--task filter the user never passed.
func TestZeroPairsRunErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: ubuntu:26.04}}}
`)

	var out bytes.Buffer
	pinAll(t, dir, cfg)
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.Error(t, err, assert.AnyError)
	assert.ContainsString(t, err.Error(), "no (agent, task) pairs to run")
}

func TestReadEgressAudit(t *testing.T) {
	assert.Equal(t, readEgressAudit(filepath.Join(t.TempDir(), "nope")).Requests, 0)

	p := filepath.Join(t.TempDir(), "egress.log")
	require.NoError(t, os.WriteFile(p, []byte(`
{"ev":"open","host":"ok.example.com","kind":"tunnel","allowed":true,"rule":"ok.example.com"}
{"ev":"close","host":"ok.example.com","allowed":true,"in":1000,"out":24}
{"ev":"open","host":"evil.example.com","allowed":false,"rule":"default"}
{"ev":"open","host":"evil.example.com","allowed":false,"rule":"default"}
{"ev":"req","host":"gh.com","url":"gh.com/x/secrets","allowed":false,"rule":"gh.com/x/*"}
{"ev":"req","host":"docs.test","url":"docs.test/private","allowed":false,"rule":"docs.test/public*","reason":"url-not-allowed"}
{"ev":"open","host":"lan.test","allowed":false,"rule":"lan.test","reason":"private"}
{"ev":"open","host":"front.test","allowed":false,"rule":"front.test","reason":"sni-mismatch"}
{"ev":"req","host":"bad.test","url":"bad.test/","allowed":false,"rule":"bad.test","reason":"revoked","status":502}
{"ev":"open","host":"down.test","allowed":false,"rule":"down.test","reason":"dial"}
{"ev":"req","host":"apt.test","url":"apt.test/x.deb","allowed":true,"rule":"apt.test","in":500,"out":10}
not json
`), 0o644))
	a := readEgressAudit(p)
	assert.EqualArrays(t, a.Denied, []string{"evil.example.com"})
	assert.EqualArrays(t, a.Blocked, []string{"gh.com/x/secrets", "docs.test/private", "lan.test", "front.test"})
	assert.EqualArrays(t, a.Failed, []string{"bad.test/", "down.test"})
	assert.Equal(t, a.Requests, 10) // open + req, close lines excluded
	assert.Equal(t, a.In, int64(1500))
	assert.Equal(t, a.Out, int64(34))
}

func TestHarnessHomeCodexCredentials(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	authDir := filepath.Join(hostHome, ".codex")
	require.NoError(t, os.MkdirAll(authDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"tokens":"secret"}`), 0o600))

	var out bytes.Buffer
	env := map[string]string{}
	hs, err := harnessHome(Options{Out: &out}, "codex x t", "codex-cli-keyless", env, false)
	require.NoError(t, err)
	defer hs.cleanup()
	require.Equal(t, len(hs.mounts), 1)
	assert.Equal(t, env["HOME"], homeMount)
	assert.Equal(t, env["CODEX_HOME"], filepath.Join(homeMount, ".codex"))
	assert.Equal(t, out.String(), "")

	copied, err := os.ReadFile(filepath.Join(hs.mounts[0].Host, ".codex", "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, string(copied), `{"tokens":"secret"}`)
	info, err := os.Stat(filepath.Join(hs.mounts[0].Host, ".codex", "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
}

func TestEgressForIncludesCodexHosts(t *testing.T) {
	sb := config.Sandbox{Egress: config.Egress{
		Set:    true,
		Policy: proxy.Policy{Default: "deny", Allow: []string{"task.example.com"}},
	}}
	eg := egressFor(sb, config.TaskEgress{}, "codex-cli-keyless", "/tmp/audit", true)
	assert.Equal(t, eg.AuditPath, "/tmp/audit")
	assert.Equal(t, eg.SelfTest, true)
	assert.EqualArrays(t, eg.Policy.Harness, []string{".openai.com", ".chatgpt.com"})
	want := map[string]bool{
		"task.example.com": true,
		".openai.com":      true,
		".chatgpt.com":     true,
	}
	for _, host := range eg.Policy.Allow {
		delete(want, host)
	}
	assert.Equal(t, len(want), 0)
}

// sandbox env reaches the agent, under what pats sets itself.
func TestNewHarnessSandboxEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sb := config.Sandbox{ID: "s", Driver: "docker", Image: "img", Env: map[string]string{"DENO_CERT": "/sbx-ca/bundle.pem", "PATS_PROMPT_FILE": "mine"}}
	a := config.Agent{ID: "a", Harness: "opencode-openrouter", Model: "m"}
	var out bytes.Buffer
	h, err := newHarness(Options{Out: &out}, a, sb, config.TaskEgress{}, t.TempDir(), "r", "", "a x t")
	require.NoError(t, err)
	defer h.cleanup()
	assert.Equal(t, h.spec.Env["DENO_CERT"], "/sbx-ca/bundle.pem")
	assert.NotEqual(t, h.spec.Env["PATS_PROMPT_FILE"], "mine")
	assert.Equal(t, sb.Env["DENO_CERT"], "/sbx-ca/bundle.pem") // the config's map is not written to
}

// a task's lists union over the sandbox's; the sandbox keeps its scalars, and
// the config's slices are never written to.
func TestEgressForTaskUnion(t *testing.T) {
	sb := config.Sandbox{Egress: config.Egress{
		Set:    true,
		Policy: proxy.Policy{Default: "deny", DenyResponse: "404", Allow: []string{"a.test"}, Deny: []string{"a.test/x*"}},
	}}
	te := config.TaskEgress{Set: true, Allow: []string{"pypi.org", "a.test"}, Deny: []string{"github.com/*/answer*"}}
	eg := egressFor(sb, te, "opencode-openrouter", "", false)
	assert.EqualArrays(t, eg.Policy.Deny, []string{"a.test/x*", "github.com/*/answer*"})
	assert.Equal(t, eg.Policy.Allow[0], "a.test")
	assert.Equal(t, eg.Policy.Allow[1], "pypi.org")
	assert.Equal(t, eg.Policy.DenyResponse, "404")
	assert.Equal(t, len(sb.Egress.Policy.Allow), 1)
	assert.Equal(t, len(sb.Egress.Policy.Deny), 1)

	// a sandbox without the proxy ignores task lists (validation refuses the pairing).
	open := egressFor(config.Sandbox{}, te, "opencode-openrouter", "", false)
	assert.Equal(t, open.Set, false)
	assert.Equal(t, len(open.Policy.Allow), 0)
}

// a failed setup step is the pair's error, named in its metadata: the agent
// never runs and nothing is collected.
func TestRunPairSetupFailureMetadata(t *testing.T) {
	for _, c := range []struct {
		name, task string
		fake       *fakeSandbox
		want       string
	}{
		{"stage", "stage: exit 3", &fakeSandbox{}, "stage: "},
		{"start", "env: {X: y}", &fakeSandbox{startErr: errors.New("no daemon")}, "start: no daemon"},
		{"prepare", "prepare: exit 4", &fakeSandbox{onRoot: func(cmd sandbox.Cmd, stdout io.Writer) (int, error) {
			if cmd.Argv[0] == "rm" {
				return 0, nil
			}
			io.WriteString(stdout, "apt said no\n")
			return 4, nil
		}}, "prepare: exited 4"},
		{"prepare cleanup", "prepare: exit 0", &fakeSandbox{onRoot: func(cmd sandbox.Cmd, stdout io.Writer) (int, error) {
			if cmd.Argv[0] == "rm" {
				return 1, nil // the material would stay where the agent can read it
			}
			return 0, nil
		}}, "remove /pats-prepare: exited 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := newSandbox
			newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return c.fake, nil }
			defer func() { newSandbox = old }()
			dir := t.TempDir()
			cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p, `+c.task+`, collect: "touch $PATS_OUTPUT_DIR/collected"}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
			var out bytes.Buffer
			pinAll(t, dir, cfg)
			runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
			require.NoError(t, err)
			var pm pairMeta
			b, err := os.ReadFile(filepath.Join(runDir, "a", "t", "metadata.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(b, &pm))
			assert.Equal(t, pm.Status, "error")
			assert.ContainsString(t, pm.Error, c.want)
			assert.Equal(t, c.fake.calls, 1) // the preflight only, never the pair's agent
			_, err = os.Stat(filepath.Join(runDir, "a", "t", "collected"))
			assert.That(t, os.IsNotExist(err), "collected after a failed setup")
			if c.name == "prepare" {
				log, _ := os.ReadFile(filepath.Join(runDir, "a", "t", "prepare.log"))
				assert.ContainsString(t, string(log), "apt said no")
			}
		})
	}
}

// prepare runs inside the sandbox as root: its material pushed in, the
// pats root's bin/ first on PATH, in-sandbox PATS_* paths, the task env, and
// the material removed again before the agent.
func TestRunPairPrepareInSandbox(t *testing.T) {
	fake := &fakeSandbox{}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "helper"), []byte("#!/bin/sh\n"), 0o755))
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p, prepare: helper --fast, env: {BRANCH: main}, stage: 'echo "$PATS_TASK_VECTOR" > "$PATS_OUTPUT_DIR/vector"'}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	var out bytes.Buffer
	pinAll(t, dir, cfg)
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	vector, err := os.ReadFile(filepath.Join(runDir, "a", "t", "vector"))
	require.NoError(t, err)
	assert.Equal(t, string(vector), "t\n") // a host step knows its task's vector
	assert.Equal(t, strings.Join(fake.pushed, " "), prepareMount)
	require.That(t, len(fake.rootCmds) >= 2, "root cmds: %v", fake.rootCmds)
	prep := fake.rootCmds[0]
	assert.Equal(t, strings.Join(prep.Argv[len(prep.Argv)-3:], " "), "/bin/sh -e "+prepareMount+"/script")
	assert.ContainsString(t, prep.Argv[2], prepareMount+"/bin:$PATH")
	assert.Equal(t, prep.Env["PATS_WORKDIR"], sandbox.WorkMount)
	assert.Equal(t, prep.Env["BRANCH"], "main")
	assert.Equal(t, prep.Env["PATS_TASK_VECTOR"], "t")
	_, hasOut := prep.Env["PATS_OUTPUT_DIR"]
	assert.Equal(t, hasOut, false)
	assert.Equal(t, strings.Join(fake.rootCmds[1].Argv, " "), "rm -rf "+prepareMount)
	assert.Equal(t, fake.calls, 2) // preflight, then the agent
}

// e2e of the in-sandbox prepare on real docker: it runs as root with the
// pats root's bin/ on PATH, what it leaves in the workdir is the agent's to
// write, and its material is gone before the agent starts.
func TestRunE2EPrepare(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "seed"), []byte("#!/bin/sh\necho \"seeded by $(id -u) for $PATS_TASK_ID\" > \"$PATS_WORKDIR/prepared.txt\"\n"), 0o755))

	old := agent.HarnessCmds["opencode-openrouter"]
	agent.HarnessCmds["opencode-openrouter"] = `echo agent >> prepared.txt && test ! -e /pats-prepare && echo material-gone >> prepared.txt`
	defer func() { agent.HarnessCmds["opencode-openrouter"] = old }()

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: ubuntu:26.04}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m1, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p, prepare: seed, collect: "cp $PATS_WORKDIR/prepared.txt $PATS_OUTPUT_DIR/"}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(runDir, "a", "t", "prepared.txt"))
	require.NoError(t, err, "%s", out.String())
	assert.Equal(t, string(got), "seeded by 0 for t\nagent\nmaterial-gone\n")
}

// e2e of base / provision on docker: the provision step runs once into a
// cached image with the pats root's bin/ on PATH, and the agent runs on it.
func TestRunE2EProvision(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "mark"), []byte("#!/bin/sh\necho \"provisioned $1\" > /etc/pats-mark\n"), 0o755))

	old := agent.HarnessCmds["opencode-openrouter"]
	agent.HarnessCmds["opencode-openrouter"] = `cat /etc/pats-mark > got.txt`
	defer func() { agent.HarnessCmds["opencode-openrouter"] = old }()

	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {base: ubuntu:26.04, provision: "mark once"}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m1, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p, collect: "cp $PATS_WORKDIR/got.txt $PATS_OUTPUT_DIR/"}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	// every run after init builds and pulls nothing, here or in the driver.
	calls := dockerSpy(t)
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, "%s", out.String())
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assertNoBuildOrPull(t, calls)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(runDir, "a", "t", "got.txt"))
	require.NoError(t, err, "%s", out.String())
	assert.Equal(t, string(got), "provisioned once\n")
	// init kept the build's whole output.
	log, err := os.ReadFile(filepath.Join(dir, ".pats", "init", "s.log"))
	require.NoError(t, err)
	assert.ContainsString(t, string(log), "load build definition from Dockerfile")
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", cfg.Sandboxes[0].Image).Run() })
}

// dockerSpy puts a docker on PATH that logs each call's arguments and runs
// the real one; it returns the log's path.
func dockerSpy(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("docker")
	require.NoError(t, err)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\nexec " + real + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// assertNoBuildOrPull fails on any build or pull among the logged calls.
func assertNoBuildOrPull(t *testing.T, log string) {
	t.Helper()
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && (f[0] == "build" || f[0] == "pull" || f[0] == "buildx" || (f[0] == "image" && len(f) > 1 && f[1] == "pull")) {
			t.Errorf("a run after init called docker %s", line)
		}
	}
	assert.That(t, len(b) > 0, "the spy saw no docker call: it is not on the path the run takes")
}
