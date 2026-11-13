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
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox"
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

// a child the script left behind, holding its output open after the script
// exited, dies with the step when the run is cancelled.
func TestStepCancelKillsLeftoverChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- step{script: "sleep 30 &\necho $! > " + pidFile + "\n", dir: dir}.run(ctx, dir, nil, io.Discard)
	}()
	var pid int
	for i := 0; i < 200 && pid == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	require.That(t, pid > 0, "the step never started its child")
	time.Sleep(100 * time.Millisecond) // the script has exited; the child holds stdout
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled step waited on its leftover child")
	}
	gone := false
	for i := 0; i < 100 && !gone; i++ {
		gone = syscall.Kill(pid, 0) != nil
		time.Sleep(10 * time.Millisecond)
	}
	assert.That(t, gone, "the leftover child outlived the cancel")
}

// a child a step started in the background, writing to the step's log file,
// does not outlive the step.
func TestStepLeftoverChildEndsWithStep(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	log, err := os.Create(filepath.Join(dir, "stage.log"))
	require.NoError(t, err)
	defer log.Close()
	err = step{script: "sleep 30 &\necho $! > " + pidFile + "\n", dir: dir}.run(context.Background(), dir, nil, log)
	require.NoError(t, err)
	b, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	require.That(t, pid > 0, "no child pid")
	gone := false
	for i := 0; i < 100 && !gone; i++ {
		gone = syscall.Kill(pid, 0) != nil
		time.Sleep(10 * time.Millisecond)
	}
	assert.That(t, gone, "the background child outlived its step")
}

// a scorer that leaves a child holding its stdout still scores at once: the
// child is killed when the script exits, before its output is drained.
func TestStepPipedLeftoverChild(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	out, err := step{script: "sleep 30 &\necho 0.5\n", dir: dir}.output(context.Background(), dir, nil)
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(out)), "0.5")
	assert.That(t, time.Since(start) < 5*time.Second, "waited %s on the leftover child", time.Since(start))

	var buf bytes.Buffer // a run's -vv tee: not a file
	start = time.Now()
	require.NoError(t, step{script: "sleep 30 &\necho staged\n", dir: dir}.run(context.Background(), dir, nil, io.MultiWriter(&buf)))
	assert.Equal(t, buf.String(), "staged\n")
	assert.That(t, time.Since(start) < 5*time.Second, "waited %s on the leftover child", time.Since(start))
}

// a child that left the step's process group and holds both its outputs is
// cut off after the delay: the step returns, failed for its truncated output,
// rather than wait on the child.
func TestStepEscapedChildCutOff(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 makes the escaping child")
	}
	old := sandbox.GroupWaitDelay
	sandbox.GroupWaitDelay = 200 * time.Millisecond
	defer func() { sandbox.GroupWaitDelay = old }()
	dir := t.TempDir()
	// the script waits until the child has left the group, so the kill at
	// its exit cannot reach it.
	escape := "python3 -c 'import os, time; os.setsid(); open(\"escaped\", \"w\").close(); time.sleep(5)' &\n" +
		"while [ ! -e escaped ]; do sleep 0.05; done\necho 0.5\n"
	done := make(chan error, 1)
	go func() {
		_, err := step{script: escape, dir: dir}.output(context.Background(), dir, nil)
		done <- err
	}()
	select {
	case err := <-done:
		assert.Error(t, err, "output cut off")
	case <-time.After(3 * time.Second):
		t.Fatal("the step waited on a child that left its group")
	}
}

// output that cannot be written fails the step instead of passing silently.
func TestStepOutputWriteFails(t *testing.T) {
	dir := t.TempDir()
	err := step{script: "echo hi\n", dir: dir}.run(context.Background(), dir, nil, failWriter{})
	assert.Error(t, err, "output:")
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
