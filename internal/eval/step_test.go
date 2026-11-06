package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// stepRoot makes a pats root with a bin/ helper and a task dir under it.
func stepRoot(t *testing.T) (root, taskDir string) {
	t.Helper()
	root = t.TempDir()
	taskDir = filepath.Join(root, "cases", "curl")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "greet"),
		[]byte("#!/bin/sh\necho \"hello $1 from $(basename \"$PWD\")\"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, "local.sh"),
		[]byte("#!/bin/sh\necho \"local $BRANCH\"\n"), 0o755))
	return root, taskDir
}

func TestStepCmd(t *testing.T) {
	root, dir := stepRoot(t)
	env := map[string]string{"BRANCH": "ubuntu-26.04"}

	// a bare first word is found on PATH, root/bin first; it runs in dir.
	out, err := step{cmd: "greet world", dir: dir}.output(root, env)
	require.NoError(t, err)
	assert.Equal(t, string(out), "hello world from curl\n")

	// a first word with a / is a file resolved against dir.
	out, err = step{cmd: "./local.sh", dir: dir}.output(root, env)
	require.NoError(t, err)
	assert.Equal(t, string(out), "local ubuntu-26.04\n")

	// not found anywhere -> a clear error.
	_, err = step{cmd: "no-such-helper", dir: dir}.output(root, nil)
	assert.Error(t, err, "command not found")

	// a non-executable file is refused.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.sh"), []byte("echo hi\n"), 0o644))
	_, err = step{cmd: "./plain.sh", dir: dir}.output(root, nil)
	assert.Error(t, err, "not executable")
}

func TestStepInline(t *testing.T) {
	root, dir := stepRoot(t)

	// no #! -> /bin/sh -e: bin/ on PATH, cwd is dir, env reaches it, ${...}
	// is the shell's, and the first failing command stops the script.
	out, err := step{inline: "greet sh\necho \"${BRANCH}\"\nfalse\necho unreachable\n", dir: dir}.output(root, map[string]string{"BRANCH": "b"})
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, string(out), "")
	out, err = step{inline: "greet sh\necho \"${BRANCH}\" \"$PATS_ROOT\"\n", dir: dir}.output(root, map[string]string{"BRANCH": "b"})
	require.NoError(t, err)
	assert.Equal(t, string(out), "hello sh from curl\nb "+root+"\n")

	// a #! line picks the interpreter; the script runs from a temp file that
	// is gone once the step returns.
	out, err = step{inline: "#!/bin/sh\necho \"$0\"\n", dir: dir}.output(root, nil)
	require.NoError(t, err)
	script := strings.TrimSpace(string(out))
	assert.ContainsString(t, filepath.Base(script), "pats-script-")
	_, err = os.Stat(script)
	assert.That(t, os.IsNotExist(err), "inline script left behind: %s", script)
}

// without a bin/ dir, PATH is the host's, untouched.
func TestHostEnvironWithoutBin(t *testing.T) {
	root := t.TempDir()
	assert.Equal(t, pathOf(hostEnviron(root, nil)), os.Getenv("PATH"))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	assert.Equal(t, pathOf(hostEnviron(root, nil)), filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}
