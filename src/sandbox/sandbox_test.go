package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
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

const testImage = "ubuntu:26.04"

func TestContainerRunMountsAndEnv(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hi"), 0o644))

	sb, err := New(Options{Driver: "docker", Image: testImage})
	require.NoError(t, err)

	var out, errb bytes.Buffer
	// proves env passthrough + cwd == mounted workdir (marker.txt visible).
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "echo $PATS_GREETING; cat marker.txt; grep NoNewPrivs /proc/self/status"},
		Workdir: dir,
		Env:     map[string]string{"PATS_GREETING": "hello-pats"},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "hello-pats")
	assert.ContainsString(t, out.String(), "hi")
	assert.ContainsString(t, out.String(), "NoNewPrivs:\t1")
}

func TestContainerExitCode(t *testing.T) {
	dockerOrSkip(t)
	sb, err := New(Options{Driver: "docker", Image: testImage})
	require.NoError(t, err)

	var out, errb bytes.Buffer
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "exit 7"},
		Workdir: t.TempDir(),
	}, &out, &errb)
	require.NoError(t, err) // ran fine; non-zero exit is reported via code, not err
	assert.Equal(t, code, 7)
}

// COVER: reproducing the ctx-cancel race that makes os/exec wrap the
// *exec.ExitError isn't reliable to trigger from a test, so this exercises
// asExitError directly: a bare `err.(*exec.ExitError)` type assertion panics
// on a wrapped ExitError; errors.As must not.
func TestAsExitErrorUnwrapsWrapped(t *testing.T) {
	raw := exec.Command("sh", "-c", "exit 3").Run()
	var rawExit *exec.ExitError
	require.That(t, errors.As(raw, &rawExit))

	wrapped := fmt.Errorf("wrapped: %w", raw)
	e, ok := asExitError(wrapped)
	require.That(t, ok)
	assert.Equal(t, e.ExitCode(), 3)

	_, ok = asExitError(errors.New("boom"))
	assert.Equal(t, ok, false)
}

func TestNewErrors(t *testing.T) {
	_, err := New(Options{Driver: "bwrap", Image: ""})
	assert.Error(t, err, "not implemented")

	_, err = New(Options{Driver: "docker", Image: ""})
	assert.Error(t, err, "needs an image")

	_, err = New(Options{Driver: "nonsense", Image: "x"})
	assert.Error(t, err, "unknown driver")
}

func TestSortedEnvStable(t *testing.T) {
	got := sortedEnv(map[string]string{"B": "2", "A": "1", "C": "3"})
	assert.EqualArrays(t, got, []string{"A=1", "B=2", "C=3"})
}

// a session: files pushed in, a root step, what it left handed to the agent
// user, the agent writing over it, and a deadline that kills the container
// rather than leaving the process running.
func TestContainerSession(t *testing.T) {
	dockerOrSkip(t)
	sb, err := New(Options{Driver: "docker", Image: testImage})
	require.NoError(t, err)
	work := t.TempDir()
	sess, err := sb.Start(context.Background(), Spec{Workdir: work})
	require.NoError(t, err)
	defer sess.Close()

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "hello"), []byte("pushed\n"), 0o644))
	require.NoError(t, sess.Push(context.Background(), src, "/pats-x"))

	var out bytes.Buffer
	code, err := sess.Exec(context.Background(), Cmd{Argv: []string{"sh", "-c", "cat /pats-x/hello > rootfile; id -u"}, Root: true}, &out, &out)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "0")
	require.NoError(t, sess.HandOver(context.Background(), WorkMount, "/no/such/path"))

	out.Reset()
	code, err = sess.Exec(context.Background(), Cmd{Argv: []string{"sh", "-c", "echo agent >> rootfile && id -u"}}, &out, &out)
	require.NoError(t, err)
	assert.Equal(t, code, 0, "agent could not write what root left: %s", out.String())
	assert.ContainsString(t, out.String(), fmt.Sprint(os.Getuid()))
	b, err := os.ReadFile(filepath.Join(work, "rootfile"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "pushed\nagent\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t0 := time.Now()
	code, _ = sess.Exec(ctx, Cmd{Argv: []string{"sleep", "60"}}, &out, &out)
	assert.That(t, time.Since(t0) < 20*time.Second, "deadline did not stop the exec")
	assert.NotEqual(t, code, 0)
}

// the copy rules the fake daemon models, on a real docker and podman: a
// directory's contents go into an existing path, and come out into hostDir.
func TestContainerCopyRules(t *testing.T) {
	for _, driver := range []string{"docker", "podman"} {
		t.Run(driver, func(t *testing.T) {
			if _, err := exec.LookPath(driver); err != nil {
				t.Skip(driver + " not on PATH")
			}
			if err := exec.Command(driver, "info").Run(); err != nil {
				t.Skip(driver + " not responding")
			}
			sb, err := New(Options{Driver: driver, Image: testImage})
			require.NoError(t, err)
			sess, err := sb.Start(context.Background(), Spec{Workdir: t.TempDir()})
			require.NoError(t, err)
			defer sess.Close()
			var out strings.Builder
			code, err := sess.Exec(context.Background(), Cmd{Argv: []string{"sh", "-c", "mkdir -p /opt/x && echo kept > /opt/x/keep"}, Root: true}, &out, &out)
			require.NoError(t, err)
			require.Equal(t, code, 0, "%s", out.String())
			src := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(src, "new"), []byte("new\n"), 0o644))
			require.NoError(t, sess.Push(context.Background(), src, "/opt/x"))
			dst := t.TempDir()
			require.NoError(t, sess.Pull(context.Background(), "/opt/x", dst))
			for name, want := range map[string]string{"new": "new\n", "keep": "kept\n"} {
				b, err := os.ReadFile(filepath.Join(dst, name))
				require.NoError(t, err, "what landed: %v", tree(dst))
				assert.Equal(t, string(b), want)
			}
		})
	}
}
