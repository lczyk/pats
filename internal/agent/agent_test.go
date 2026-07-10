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

func TestSpecCodexCliKeyless(t *testing.T) {
	spec, err := Spec(config.Agent{ID: "c", Kind: "codex-cli-keyless", Model: "gpt-5", Effort: "high", Sandbox: "s"}, "/tmp", nil)
	require.NoError(t, err)
	cmd := spec.Argv[len(spec.Argv)-1]
	assert.ContainsString(t, cmd, "codex exec --json --ephemeral")
	assert.ContainsString(t, cmd, "--ignore-user-config")
	assert.ContainsString(t, cmd, "--dangerously-bypass-approvals-and-sandbox")
	assert.ContainsString(t, cmd, `model_reasoning_effort="$PATS_EFFORT"`)
	assert.ContainsString(t, cmd, `< "$PATS_PROMPT_FILE"`)
}

// a harness inherits only its own provider's env: an OPENROUTER_API_KEY on the
// host must not reach claude, nor an ANTHROPIC_API_KEY reach opencode. codex is
// keyless with no env path at all, so an OPENAI_API_KEY must not reach it either
// -- that's what stops it quietly authenticating as an api-key account.
func TestCredEnvIsKindSpecific(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("OPENROUTER_API_KEY", "openrouter-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")

	claude, hasToken := CredEnv("claude-cli-keyless")
	assert.Equal(t, claude["ANTHROPIC_API_KEY"], "anthropic-secret")
	assert.Equal(t, hasToken, true)
	if _, ok := claude["OPENROUTER_API_KEY"]; ok {
		t.Error("claude inherited an unrelated openrouter credential")
	}

	openrouter, hasToken := CredEnv("opencode-openrouter")
	assert.Equal(t, openrouter["OPENROUTER_API_KEY"], "openrouter-secret")
	assert.Equal(t, hasToken, true)
	if _, ok := openrouter["ANTHROPIC_API_KEY"]; ok {
		t.Error("opencode inherited an unrelated anthropic credential")
	}

	codex, hasToken := CredEnv("codex-cli-keyless")
	assert.Equal(t, len(codex), 0)
	assert.Equal(t, hasToken, false)

	// an unknown kind inherits nothing rather than everything.
	unknown, hasToken := CredEnv("no-such-kind")
	assert.Equal(t, len(unknown), 0)
	assert.Equal(t, hasToken, false)
}

func TestHostCredsFileCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	want := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(want, []byte("{}"), 0o600))

	host, rel := HostCredsFile("codex-cli-keyless")
	assert.Equal(t, host, want)
	assert.Equal(t, rel, filepath.Join(".codex", "auth.json"))
}

func TestSpecUnsupportedKind(t *testing.T) {
	_, err := Spec(config.Agent{ID: "h", Kind: "codex-cli", Sandbox: "s"}, "/tmp", nil)
	assert.Error(t, err, "unsupported kind")
}

// the kind registry is spelled out twice (config.AgentKinds for validation,
// HarnessCmds for execution -- an import cycle keeps them apart); this pins
// them to the same key set.
func TestKindRegistriesMatch(t *testing.T) {
	for k := range HarnessCmds {
		if !config.AgentKinds[k] {
			t.Errorf("HarnessCmds kind %q missing from config.AgentKinds", k)
		}
	}
	for k := range config.AgentKinds {
		if _, ok := HarnessCmds[k]; !ok {
			t.Errorf("config.AgentKinds kind %q missing from HarnessCmds", k)
		}
	}
}
