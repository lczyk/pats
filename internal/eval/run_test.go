package eval

import (
	"bytes"
	"context"
	"errors"
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
// kind's command is overridden with a no-cred stand-in.
func TestRunE2E(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("do the thing"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "collect.sh"),
		[]byte("#!/bin/sh\ncp \"$PATS_WORKDIR/result.txt\" \"$PATS_OUTPUT_DIR/\"\n"), 0o755))

	old := agent.HarnessCmds["opencode-openrouter"]
	agent.HarnessCmds["opencode-openrouter"] = `cat "$PATS_PROMPT_FILE" > result.txt; echo "model=$PATS_MODEL"`
	defer func() { agent.HarnessCmds["opencode-openrouter"] = old }()

	cfg := &config.Config{
		Sandboxes: []config.Sandbox{{ID: "s", Kind: "container", Driver: "docker", Image: "ubuntu:26.04"}},
		Agents: []config.Agent{{
			ID: "a", Kind: "opencode-openrouter", Model: "m1", Sandbox: "s",
		}},
		Tasks:  []config.Task{{ID: "t", Prompt: "prompt.txt", Collect: "collect.sh"}},
		Suites: []config.Suite{{ID: "su", Agents: config.StrList{"a"}, Tasks: config.StrList{"t"}}},
	}
	require.NoError(t, cfg.Validate())

	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
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
	assert.Equal(t, resolveJobs(4), 4) // explicit count passes through
	assert.Equal(t, resolveJobs(1), 1) // serial
	assert.Equal(t, resolveJobs(0), 1) // zero-value -> serial
	// auto (<0): docker cpu or host-cpu fallback, clamped to [1,8].
	got := resolveJobs(-1)
	assert.That(t, got >= 1 && got <= 8, "auto jobs in [1,8], got", got)
}

func TestResolvePrompt(t *testing.T) {
	dir := t.TempDir()
	// plain file -> contents.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.txt"), []byte("from file\n"), 0o644))
	got, err := resolvePrompt(dir, "p.txt", nil)
	require.NoError(t, err)
	assert.Equal(t, string(got), "from file\n")

	// executable file -> its stdout, with env + args passed through.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gen.sh"),
		[]byte("#!/bin/sh\necho \"gen $PATS_TASK_ID $1\"\n"), 0o755))
	got, err = resolvePrompt(dir, "gen.sh --flavour spicy", map[string]string{"PATS_TASK_ID": "t1"})
	require.NoError(t, err)
	assert.Equal(t, string(got), "gen t1 --flavour\n")

	// not a file -> the spec is the literal prompt (even with quotes/apostrophes).
	got, err = resolvePrompt(dir, "just write a readme", nil)
	require.NoError(t, err)
	assert.Equal(t, string(got), "just write a readme")
	got, err = resolvePrompt(dir, "it's a literal prompt", nil) // bad quoting -> literal
	require.NoError(t, err)
	assert.Equal(t, string(got), "it's a literal prompt")

	// non-executable file named WITH args -> error, not a silent arg-drop.
	_, err = resolvePrompt(dir, "p.txt --flag", nil)
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
}

func (f *fakeSandbox) Run(ctx context.Context, spec sandbox.Spec, stdout, stderr io.Writer) (int, error) {
	// first call per agent is the credential preflight -- always passes so the
	// canned outcome lands on the real pair run.
	if f.calls++; f.calls == 1 {
		return 0, nil
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
	newSandbox = func(driver, image string) (sandbox.Sandbox, error) { return fakes[image], nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))

	var sbs []config.Sandbox
	var agents []config.Agent
	var ids config.StrList
	for id := range fakes {
		sbs = append(sbs, config.Sandbox{ID: id, Kind: "container", Driver: "docker", Image: id})
		agents = append(agents, config.Agent{ID: id, Kind: "opencode-openrouter", Model: "m", Sandbox: id})
		ids = append(ids, id)
	}
	cfg := &config.Config{
		Sandboxes: sbs,
		Agents:    agents,
		Tasks:     []config.Task{{ID: "t", Prompt: "prompt.txt"}},
		Suites:    []config.Suite{{ID: "su", Agents: ids, Tasks: config.StrList{"t"}}},
	}
	require.NoError(t, cfg.Validate())

	var out bytes.Buffer
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

func TestDeniedEgress(t *testing.T) {
	assert.That(t, deniedEgress(filepath.Join(t.TempDir(), "nope")) == nil, "missing file -> nil")

	p := filepath.Join(t.TempDir(), "egress.log")
	require.NoError(t, os.WriteFile(p, []byte(`
{"host":"ok.example.com","allowed":true}
{"host":"evil.example.com","allowed":false}
{"host":"evil.example.com","allowed":false}
{"host":"gh.com","url":"gh.com/x/secrets","allowed":false}
not json
`), 0o644))
	got := deniedEgress(p)
	// unique targets, url preferred over host, allowed + malformed skipped.
	want := []string{"evil.example.com", "gh.com/x/secrets"}
	require.Equal(t, len(got), len(want))
	for i := range want {
		assert.Equal(t, got[i], want[i])
	}
}

func TestHarnessHomeCodexCredentials(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	authDir := filepath.Join(hostHome, ".codex")
	require.NoError(t, os.MkdirAll(authDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"tokens":"secret"}`), 0o600))

	var out bytes.Buffer
	env := map[string]string{}
	hs, err := harnessHome(Options{Out: &out}, config.TestPair{Agent: "codex", Task: "t"}, "codex-cli-keyless", env, false)
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
		Mode:    "proxy",
		Default: "deny",
		Allow:   []string{"task.example.com"},
	}}
	eg := egressFor(sb, "codex-cli-keyless", "/tmp/audit")
	assert.Equal(t, eg.AuditPath, "/tmp/audit")
	want := map[string]bool{
		"task.example.com": true,
		".openai.com":      true,
		".chatgpt.com":     true,
	}
	for _, host := range eg.Allow {
		delete(want, host)
	}
	assert.Equal(t, len(want), 0)
}
