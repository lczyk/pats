package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the script sets up the tools and no harness cli: pats places those. it
// needs nothing beside itself, and the dockerfile passes it no build arg.
// apt-get and rm are stubs that log their arguments; npm is one that must
// not be called.
func TestProvisionInstallsNoHarness(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "provision-sandbox.sh"), []byte(ProvisionSandbox), 0o755))
	stubs := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	for _, name := range []string{"apt-get", "npm", "rm"} {
		stub := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(stubs, name), []byte(stub), 0o755))
	}
	cmd := exec.Command(sh, filepath.Join(dir, "provision-sandbox.sh"))
	cmd.Env = []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	calls := string(b)
	for _, tool := range []string{"ca-certificates", "curl", "git", "nodejs", "npm", "python3", "ripgrep"} {
		assert.ContainsString(t, calls, " "+tool)
	}
	for line := range strings.SplitSeq(calls, "\n") {
		assert.That(t, !strings.HasPrefix(line, "npm "), "the script ran npm: %s", line)
	}

	dockerfile, err := os.ReadFile("Dockerfile.sandbox-26.04")
	require.NoError(t, err)
	assert.That(t, !strings.Contains(string(dockerfile), "\nARG "), "the sandbox dockerfile takes a build arg")
}
