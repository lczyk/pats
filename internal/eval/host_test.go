//go:build !nohost

package eval

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// a host sandbox's run: warned about before the first pair, the machine's
// harness versions recorded, the pair marked unboxed with advisory egress,
// and the report saying so.
func TestRunHostRecords(t *testing.T) {
	fake := &fakeSandbox{}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()
	// preflight looks for the harness cli on PATH; the fake sandbox never
	// runs it.
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, egress: {allow: [a.test]}}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	h := pinAll(t, dir, cfg)
	h.stamped = map[string]string{"opencode": "0.0.1"}
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1})
	require.NoError(t, err)
	text := out.String()
	assert.ContainsString(t, text, `sandbox "sxx" runs agents on this machine, unsandboxed (sand-not-boxed); egress is advisory`)
	assert.ContainsString(t, text, "this machine's harnesses differ from what is pinned: opencode 0.0.1")

	b, err := os.ReadFile(filepath.Join(runDir, "axx", "t1x", "metadata.json"))
	require.NoError(t, err)
	var m pairMeta
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, m.Driver, "host")
	assert.Equal(t, m.SandNotBoxed, true)
	require.NotNil(t, m.Egress)
	require.NotNil(t, m.Egress.Enforced)
	assert.Equal(t, *m.Egress.Enforced, false)
	assert.Equal(t, m.Components["opencode"], "0.0.1")

	var rep bytes.Buffer
	report(&rep, runDir, &ScoreReport{PerPair: map[string]float64{"axx/t1x": 1}}, false, nil)
	assert.ContainsString(t, rep.String(), "egress advisory (host driver, unsandboxed; an empty denied list is not clean): axx/t1x")
	doc := buildReport(runDir, &ScoreReport{}, nil)
	require.Equal(t, len(doc.Pairs), 1)
	require.NotNil(t, doc.Pairs[0].EgressEnforced)
	assert.Equal(t, *doc.Pairs[0].EgressEnforced, false)
	assert.That(t, !strings.Contains(rep.String(), "judge"), "no judges here: %s", rep.String())
}

// under host the agent is told its dirs as they are on this machine.
func TestHostHarnessPaths(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	work := t.TempDir()
	var out bytes.Buffer
	h, err := newHarness(Options{Out: &out}, cfg.Agents[0], cfg.HarnessOf(cfg.Agents[0]), cfg.Sandboxes[0], config.TaskEgress{}, work, "r", "", "axx x t1x")
	require.NoError(t, err)
	defer h.cleanup()
	assert.Equal(t, h.paths.Work, work)
	assert.Equal(t, h.paths.Home, h.homeDir)
	assert.Equal(t, h.spec.Env["HOME"], h.homeDir)
	assert.Equal(t, h.spec.Env["PATS_PROMPT_FILE"], h.promptPath())
	env := prepareEnv(pairJob{agent: cfg.Agents[0]}, "r", h.paths)
	assert.Equal(t, env["PATS_WORKDIR"], work)
	assert.Equal(t, env["PATS_AGENT_HOME"], h.homeDir)
}

// a real host run, its harness a stand-in on PATH: the deadline kills the
// agent and its children, the pair is a timeout and collect still runs; a
// missing cli fails preflight with its name.
func TestHostRunLifecycle(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := t.TempDir()
	// preflight's prompt answers at once; a task's sleeps past its deadline,
	// leaving a child behind.
	stand := "#!/bin/sh\ncase \"$(cat \"$PATS_PROMPT_FILE\")\" in *'single word: OK'*) echo OK; exit 0;; esac\n" +
		"sleep 300 & echo $! > child.pid; wait\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "opencode"), []byte(stand), 0o755))
	// system dirs only: a real harness cli on this machine must not answer.
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p, timeout: 2s, collect: "cp \"$PATS_WORKDIR/child.pid\" \"$PATS_OUTPUT_DIR/collected\""}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Offline: true})
	require.NoError(t, err, out.String())
	b, err := os.ReadFile(filepath.Join(runDir, "axx", "t1x", "metadata.json"))
	require.NoError(t, err)
	var m pairMeta
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, m.Status, "timeout")
	pid, err := os.ReadFile(filepath.Join(runDir, "axx", "t1x", "collected"))
	require.NoError(t, err, "collect did not run: %s", out.String())
	assert.That(t, exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() != nil, "the agent's child outlived the pair")

	require.NoError(t, os.Remove(filepath.Join(bin, "opencode")))
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Offline: true})
	assert.Error(t, err, "its harness cli opencode is not on this machine's PATH")
}

// ctrl-c during a real host run ends the agent and its child, records the
// pair interrupted, and runs no collect.
func TestHostRunInterrupted(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "child.pid")
	stand := "#!/bin/sh\ncase \"$(cat \"$PATS_PROMPT_FILE\")\" in *'single word: OK'*) echo OK; exit 0;; esac\n" +
		"sleep 300 & echo $! > '" + pidFile + "'; wait\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "opencode"), []byte(stand), 0o755))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tvx: {kind: task, items: {t1x: {prompt: p, timeout: 60s, collect: "touch \"$PATS_OUTPUT_DIR/collected\""}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Jobs: 1, Offline: true})
	assert.ErrorIs(t, err, ErrInterrupted)
	b, rerr := os.ReadFile(filepath.Join(runDir, "axx", "t1x", "metadata.json"))
	require.NoError(t, rerr)
	var m pairMeta
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, m.Error, "interrupted")
	_, cerr := os.Stat(filepath.Join(runDir, "axx", "t1x", "collected"))
	assert.That(t, os.IsNotExist(cerr), "collect ran after ctrl-c")
	pid, rerr := os.ReadFile(pidFile)
	require.NoError(t, rerr)
	assert.That(t, exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() != nil, "the agent's child outlived ctrl-c")
}
