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
	"github.com/lczyk/pats/internal/machine"
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
// the harness's command is overridden with a no-cred stand-in so the test needs
// no real cli/creds.
func TestHarnessThroughSandbox(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()

	hd := entry("opencode-openrouter")
	hd.Run = `echo "model=$PATS_MODEL"; echo "prompt=$PATS_PROMPT_FILE"; echo hello > out.txt`

	a := config.Agent{Name: "x", Harness: "opencode-openrouter", Model: "m1", Sandbox: "s"}
	env := Env(a, sandbox.WorkMount+"/prompt.txt", sandbox.WorkMount, "")
	spec, err := Spec(hd, a, dir, env)
	require.NoError(t, err)

	d, err := sandbox.Open(sandbox.Options{Driver: "docker"})
	require.NoError(t, err)
	sb, err := d.Sandbox(sandbox.SandboxOptions{Image: "ubuntu:26.04"})
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

// entry is a default harness entry, the test's own copy; nil for an unknown
// name.
func entry(name string) *machine.Harness { return machine.Defaults().Harness(name) }

func TestSpecClaudeCli(t *testing.T) {
	spec, err := Spec(entry("claude-cli-keyless"), config.Agent{Name: "h", Harness: "claude-cli-keyless", Sandbox: "s"}, "/tmp", nil)
	require.NoError(t, err)
	assert.Equal(t, spec.Argv[0], "sh")
	assert.ContainsString(t, spec.Argv[len(spec.Argv)-1], "claude --print")
}

func TestSpecCodexCliKeyless(t *testing.T) {
	spec, err := Spec(entry("codex-cli-keyless"), config.Agent{Name: "c", Harness: "codex-cli-keyless", Model: "gpt-5", Effort: "high", Sandbox: "s"}, "/tmp", nil)
	require.NoError(t, err)
	cmd := spec.Argv[len(spec.Argv)-1]
	assert.ContainsString(t, cmd, "codex exec --json --ephemeral")
	assert.ContainsString(t, cmd, "--ignore-user-config")
	assert.ContainsString(t, cmd, "--dangerously-bypass-approvals-and-sandbox")
	assert.ContainsString(t, cmd, `model_reasoning_effort="$PATS_EFFORT"`)
	assert.ContainsString(t, cmd, `< "$PATS_PROMPT_FILE"`)
}

func TestSpecCopilotCliKeyless(t *testing.T) {
	spec, err := Spec(entry("copilot-cli-keyless"), config.Agent{Name: "p", Harness: "copilot-cli-keyless", Model: "gpt-5", Effort: "high", Sandbox: "s"}, "/tmp", nil)
	require.NoError(t, err)
	cmd := spec.Argv[len(spec.Argv)-1]
	assert.ContainsString(t, cmd, "copilot -p")
	assert.ContainsString(t, cmd, "--allow-all")
	assert.ContainsString(t, cmd, "--output-format json")
	assert.ContainsString(t, cmd, "--no-auto-update")
	assert.ContainsString(t, cmd, "--disable-builtin-mcps")
	assert.ContainsString(t, cmd, `--effort "$PATS_EFFORT"`)
}

// a harness inherits only its own provider's env: an OPENROUTER_API_KEY on the
// host must not reach claude, nor an ANTHROPIC_API_KEY reach opencode. codex is
// keyless with no env path at all, so an OPENAI_API_KEY must not reach it either
// -- that's what stops it quietly authenticating as an api-key account. copilot
// takes only its own COPILOT_GITHUB_TOKEN: an ambient GITHUB_TOKEN (ci, other
// tooling) must not swap the harness's identity behind your back.
func TestCredEnvIsKindSpecific(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("OPENROUTER_API_KEY", "openrouter-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-secret")
	t.Setenv("GITHUB_TOKEN", "github-ambient")

	claude, hasToken := CredEnv(entry("claude-cli-keyless"))
	assert.Equal(t, claude["ANTHROPIC_API_KEY"], "anthropic-secret")
	assert.Equal(t, hasToken, true)
	if _, ok := claude["OPENROUTER_API_KEY"]; ok {
		t.Error("claude inherited an unrelated openrouter credential")
	}

	openrouter, hasToken := CredEnv(entry("opencode-openrouter"))
	assert.Equal(t, openrouter["OPENROUTER_API_KEY"], "openrouter-secret")
	assert.Equal(t, hasToken, true)
	if _, ok := openrouter["ANTHROPIC_API_KEY"]; ok {
		t.Error("opencode inherited an unrelated anthropic credential")
	}

	codex, hasToken := CredEnv(entry("codex-cli-keyless"))
	assert.Equal(t, len(codex), 0)
	assert.Equal(t, hasToken, false)

	copilot, hasToken := CredEnv(entry("copilot-cli-keyless"))
	assert.Equal(t, copilot["COPILOT_GITHUB_TOKEN"], "copilot-secret")
	assert.Equal(t, hasToken, true)
	assert.Equal(t, len(copilot), 1) // GITHUB_TOKEN stays behind

	// an unknown kind inherits nothing rather than everything.
	unknown, hasToken := CredEnv(entry("no-such-kind"))
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

	t.Setenv("PATS_CODEX_AUTH_PATH", "")
	t.Setenv("SNAP_REAL_HOME", "")
	host, rel, err := HostCredsFile(entry("codex-cli-keyless"))
	require.NoError(t, err)
	assert.Equal(t, host, want)
	assert.Equal(t, rel, filepath.Join(".codex", "auth.json"))
}

// the auth path variable names the login file, in every build; one that
// cannot be read is an error naming it, never the default instead.
func TestHostCredsFileAuthPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SNAP_REAL_HOME", "")
	login := filepath.Join(t.TempDir(), "codex.json")
	require.NoError(t, os.WriteFile(login, []byte("{}"), 0o600))
	t.Setenv("PATS_CODEX_AUTH_PATH", login)
	host, rel, err := HostCredsFile(entry("codex-cli-keyless"))
	require.NoError(t, err)
	assert.Equal(t, host, login)
	assert.Equal(t, rel, filepath.Join(".codex", "auth.json"))

	t.Setenv("PATS_CLAUDE_AUTH_PATH", filepath.Join(t.TempDir(), "missing"))
	_, _, err = HostCredsFile(entry("claude-cli-keyless"))
	assert.Error(t, err, "PATS_CLAUDE_AUTH_PATH=")
}

// a strict snap's HOME is its own: the login is looked for in the real one.
func TestHostCredsFileRealHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	real := t.TempDir()
	t.Setenv("SNAP_REAL_HOME", real)
	t.Setenv("PATS_CLAUDE_AUTH_PATH", "")
	require.NoError(t, os.MkdirAll(filepath.Join(real, ".claude"), 0o700))
	want := filepath.Join(real, ".claude", ".credentials.json")
	require.NoError(t, os.WriteFile(want, []byte("{}"), 0o600))
	host, _, err := HostCredsFile(entry("claude-cli-keyless"))
	require.NoError(t, err)
	assert.Equal(t, host, want)
}

// a default login this process may not read is an error naming the
// variable that points at a copy.
func TestHostCredsFileUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a 0 mode file")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SNAP_REAL_HOME", "")
	t.Setenv("PATS_CODEX_AUTH_PATH", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o000))
	_, _, err := HostCredsFile(entry("codex-cli-keyless"))
	assert.Error(t, err, "set PATS_CODEX_AUTH_PATH")
}

func TestSpecUnsupportedHarness(t *testing.T) {
	_, err := Spec(entry("codex-cli"), config.Agent{Name: "h", Harness: "codex-cli", Sandbox: "s"}, "/tmp", nil)
	assert.Error(t, err, "unsupported harness")
}

// every default must make an explicit choice of each thing an agent run
// needs. no hosts passes in egress allow mode and then fails every
// deny-default proxy run with an opaque auth error; no credential source
// leaves a harness that cannot log in; a fallback CredentialHint makes the
// no-creds warning useless.
func TestDefaultsComplete(t *testing.T) {
	for _, name := range machine.Defaults().Names() {
		hd := entry(name)
		if len(hd.Egress.Allow) == 0 {
			t.Errorf("harness %q allows itself no host", name)
		}
		if len(hd.AuthEnv) == 0 && hd.LoginFile() == "" {
			t.Errorf("harness %q has neither an auth-env nor a login file", name)
		}
		if CredentialHint(hd) == CredentialHint(nil) {
			t.Errorf("harness %q falls through to the default CredentialHint", name)
		}
	}
}

// a hint is the entry's own, else its variables and its login file.
func TestCredentialHint(t *testing.T) {
	assert.Equal(t, CredentialHint(entry("opencode-openrouter")), "OPENROUTER_API_KEY")
	assert.ContainsString(t, CredentialHint(entry("claude-cli-keyless")), "PATS_CLAUDE_AUTH_PATH")
	hd := entry("codex-cli-keyless")
	hd.AuthHint = ""
	assert.Equal(t, CredentialHint(hd), "~/.codex/auth.json (or PATS_CODEX_AUTH_PATH)")
}

// a cli's home-env is joined to the harness home, and a pass-env variable is
// forwarded without counting as a credential.
func TestHomeEnvAndPassEnv(t *testing.T) {
	assert.Equal(t, HomeEnv(entry("codex-cli-keyless"), "/h")["CODEX_HOME"], filepath.Join("/h", ".codex"))
	assert.Equal(t, len(HomeEnv(entry("claude-cli-keyless"), "/h")), 0)

	for _, k := range entry("claude-cli-keyless").AuthEnv {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:1")
	env, hasToken := CredEnv(entry("claude-cli-keyless"))
	assert.Equal(t, env["ANTHROPIC_BASE_URL"], "http://localhost:1")
	assert.Equal(t, hasToken, false)
}
