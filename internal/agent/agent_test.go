package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/sandbox"
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

// first sandboxed e2e of the agent layer: a harness runs in docker, sees its
// PATS_* env, and writes a file into the mounted workdir (visible on host).
// the kind's command is overridden with a no-cred stand-in so the test needs
// no real cli/creds.
func TestHarnessThroughSandbox(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()

	defer overrideCmd("opencode-openrouter",
		`echo "model=$PATS_MODEL"; echo "prompt=$PATS_PROMPT_FILE"; echo hello > out.txt`)()

	a := config.Agent{ID: "x", Kind: "opencode-openrouter", Model: "m1", Sandbox: "s"}
	env := Env(a, sandbox.WorkMount+"/prompt.txt", sandbox.WorkMount)
	spec, err := Spec(a, dir, env)
	require.NoError(t, err)

	sb, err := sandbox.New("docker", "ubuntu:26.04")
	require.NoError(t, err)

	var out, errb bytes.Buffer
	code, err := sb.Run(context.Background(), spec, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "model=m1")
	assert.ContainsString(t, out.String(), "prompt=/workspace/prompt.txt")

	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	require.NoError(t, err)
	assert.ContainsString(t, string(b), "hello")
}

// overrideCmd swaps a kind's command for the duration of a test; the returned
// func restores it.
func overrideCmd(kind, cmd string) func() {
	old := HarnessCmds[kind]
	HarnessCmds[kind] = cmd
	return func() { HarnessCmds[kind] = old }
}

func TestSpecClaudeCli(t *testing.T) {
	spec, err := Spec(config.Agent{ID: "h", Kind: "claude-cli-keyless", Sandbox: "s"}, "/tmp", nil)
	require.NoError(t, err)
	assert.Equal(t, spec.Argv[0], "sh")
	assert.ContainsString(t, spec.Argv[len(spec.Argv)-1], "claude --print")
}

func TestSpecUnsupportedKind(t *testing.T) {
	_, err := Spec(config.Agent{ID: "h", Kind: "codex-cli", Sandbox: "s"}, "/tmp", nil)
	assert.Error(t, err, "unsupported kind")
}
