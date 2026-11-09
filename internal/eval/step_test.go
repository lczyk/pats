package eval

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

// a one-line step is a script like any other: sh finds a bare word on
// PATH, root/bin first, and a path against the step's dir, and passes the
// rest as arguments, with env expanded.
func TestStepOneLiner(t *testing.T) {
	root, dir := stepRoot(t)
	env := map[string]string{"BRANCH": "ubuntu-26.04"}

	out, err := step{script: "greet world", dir: dir}.output(context.Background(), root, env)
	require.NoError(t, err)
	assert.Equal(t, string(out), "hello world from curl\n")

	out, err = step{script: "./local.sh", dir: dir}.output(context.Background(), root, env)
	require.NoError(t, err)
	assert.Equal(t, string(out), "local ubuntu-26.04\n")

	out, err = step{script: `greet "$BRANCH"`, dir: dir}.output(context.Background(), root, env)
	require.NoError(t, err)
	assert.Equal(t, string(out), "hello ubuntu-26.04 from curl\n")

	// not found anywhere, or not executable: the shell's error.
	_, err = step{script: "no-such-helper", dir: dir}.output(context.Background(), root, nil)
	assert.Error(t, err, "not found")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.sh"), []byte("echo hi\n"), 0o644))
	_, err = step{script: "./plain.sh", dir: dir}.output(context.Background(), root, nil)
	assert.Error(t, err, "denied")
}

func TestStepInline(t *testing.T) {
	root, dir := stepRoot(t)

	// no #! -> /bin/sh -e: bin/ on PATH, cwd is dir, env reaches it, ${...}
	// is the shell's, and the first failing command stops the script.
	out, err := step{script: "greet sh\necho \"${BRANCH}\"\nfalse\necho unreachable\n", dir: dir}.output(context.Background(), root, map[string]string{"BRANCH": "b"})
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, string(out), "")
	out, err = step{script: "greet sh\necho \"${BRANCH}\" \"$PATS_ROOT\"\n", dir: dir}.output(context.Background(), root, map[string]string{"BRANCH": "b"})
	require.NoError(t, err)
	assert.Equal(t, string(out), "hello sh from curl\nb "+root+"\n")

	// a #! line picks the interpreter; the script runs from a temp file that
	// is gone once the step returns.
	out, err = step{script: "#!/bin/sh\necho \"$0\"\n", dir: dir}.output(context.Background(), root, nil)
	require.NoError(t, err)
	script := strings.TrimSpace(string(out))
	assert.ContainsString(t, filepath.Base(script), "pats-script-")
	_, err = os.Stat(script)
	assert.That(t, os.IsNotExist(err), "inline script left behind: %s", script)
}

// without a bin/ dir, PATH is the host's, untouched.
func TestHostEnvironWithoutBin(t *testing.T) {
	pathOf := func(environ []string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(environ[i], "PATH="); ok {
				return v
			}
		}
		return ""
	}
	root := t.TempDir()
	assert.Equal(t, pathOf(hostEnviron(root, nil)), os.Getenv("PATH"))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	assert.Equal(t, pathOf(hostEnviron(root, nil)), filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// a step sees its own map's names only: a parent pats's step vars, inherited
// through the environment, never leak into a step that leaves them out.
func TestHostEnvironDropsInheritedStepNames(t *testing.T) {
	t.Setenv("PATS_TASK_NAME", "parent-task")
	t.Setenv("PATS_OUTPUT_DIR", "/parent/out")
	t.Setenv("PATS_LXD_UPLINK", "keep")
	env := hostEnviron(t.TempDir(), map[string]string{"PATS_SCORER_NAME": "own"})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	_, task := got["PATS_TASK_NAME"]
	_, out := got["PATS_OUTPUT_DIR"]
	assert.That(t, !task && !out, "inherited step names kept")
	assert.Equal(t, got["PATS_LXD_UPLINK"], "keep")
	assert.Equal(t, got["PATS_SCORER_NAME"], "own")
}

// a cancelled step is killed with everything it started, at once.
func TestStepCancelKillsGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- step{script: "sleep 30 &\necho $! > " + pidFile + "\nwait\n", dir: dir}.run(ctx, dir, nil, io.Discard)
	}()
	var pid int
	for i := 0; i < 200 && pid == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	require.That(t, pid > 0, "the step never started its child")
	cancel()
	select {
	case err := <-done:
		assert.That(t, err != nil, "a killed step reported success")
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled step kept running")
	}
	gone := false
	for i := 0; i < 100 && !gone; i++ {
		gone = syscall.Kill(pid, 0) != nil
		time.Sleep(10 * time.Millisecond)
	}
	assert.That(t, gone, "the step's child outlived it")
}
