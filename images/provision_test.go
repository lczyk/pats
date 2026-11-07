package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// pins parses HarnessVersions the way the provision script does.
func pins(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(HarnessVersions, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ver, ok := strings.Cut(line, "=")
		require.That(t, ok, "pin line without '=': %q", line)
		out[name] = ver
	}
	return out
}

// every pin is an exact version, and every pin reaches both the script's
// install line and the dockerfile's build args.
func TestHarnessVersionsShape(t *testing.T) {
	exact := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	dockerfile, err := os.ReadFile("Dockerfile.sandbox-26.04")
	require.NoError(t, err)
	p := pins(t)
	assert.Equal(t, len(p), 4)
	assert.Equal(t, len(ComponentPins()), len(p)) // every pin names a component
	for name, ver := range p {
		assert.That(t, exact.MatchString(ver), "%s=%s is not an exact version", name, ver)
		assert.ContainsString(t, ProvisionSandbox, "${"+name+":?no pin}")
		assert.ContainsString(t, string(dockerfile), "ARG "+name+"\n")
	}
}

// the script installs the pinned versions, an env value overrides its pin,
// an empty one (an unset build arg) does not, and a missing pins file stops
// it. apt-get, npm and rm are stubs that log their arguments.
func TestProvisionReadsPins(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	run := func(t *testing.T, withPins bool, env ...string) (string, error) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "provision-sandbox.sh"), []byte(ProvisionSandbox), 0o755))
		if withPins {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "harness-versions"), []byte(HarnessVersions), 0o644))
		}
		stubs := t.TempDir()
		log := filepath.Join(t.TempDir(), "calls")
		for _, name := range []string{"apt-get", "npm", "rm"} {
			stub := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(stubs, name), []byte(stub), 0o755))
		}
		cmd := exec.Command(sh, filepath.Join(dir, "provision-sandbox.sh"))
		cmd.Env = append([]string{"PATH=" + stubs + ":" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(log)
		return string(calls) + string(out), err
	}
	p := pins(t)

	t.Run("pinned", func(t *testing.T) {
		out, err := run(t, true, "CLAUDE_CLI_VERSION=")
		require.NoError(t, err, "%s", out)
		assert.ContainsString(t, out, "@anthropic-ai/claude-code@"+p["CLAUDE_CLI_VERSION"])
		assert.ContainsString(t, out, "@github/copilot@"+p["COPILOT_CLI_VERSION"])
		assert.ContainsString(t, out, "@openai/codex@"+p["CODEX_CLI_VERSION"])
		assert.ContainsString(t, out, "opencode-ai@"+p["OPENCODE_VERSION"])
	})
	t.Run("an env value wins", func(t *testing.T) {
		out, err := run(t, true, "CODEX_CLI_VERSION=9.9.9")
		require.NoError(t, err, "%s", out)
		assert.ContainsString(t, out, "@openai/codex@9.9.9")
		assert.ContainsString(t, out, "@anthropic-ai/claude-code@"+p["CLAUDE_CLI_VERSION"])
	})
	t.Run("no pins file", func(t *testing.T) {
		out, err := run(t, false)
		assert.That(t, err != nil, "provisioned without a pins file")
		assert.ContainsString(t, out, "harness-versions is missing")
		assert.That(t, !strings.Contains(out, "npm install"), "installed without pins: %s", out)
	})
}
