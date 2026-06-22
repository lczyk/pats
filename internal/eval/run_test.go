package eval

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
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
		Tasks:      []config.Task{{ID: "t", PromptFile: "prompt.txt", Collect: "sh collect.sh"}},
		TestMatrix: []config.Row{{Agent: config.StrList{"a"}, Task: config.StrList{"t"}}},
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
	assert.Equal(t, filepath.Base(d1), "20260621-1")
	d2, err := nextRunDir(base, now)
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(d2), "20260621-2")
}
