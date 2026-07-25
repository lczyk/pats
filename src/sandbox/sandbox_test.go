package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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

	sb, err := New("docker", testImage)
	require.NoError(t, err)

	var out, errb bytes.Buffer
	// proves env passthrough + cwd == mounted workdir (marker.txt visible).
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "echo $PATS_GREETING; cat marker.txt"},
		Workdir: dir,
		Env:     map[string]string{"PATS_GREETING": "hello-pats"},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "hello-pats")
	assert.ContainsString(t, out.String(), "hi")
}

func TestContainerExitCode(t *testing.T) {
	dockerOrSkip(t)
	sb, err := New("docker", testImage)
	require.NoError(t, err)

	var out, errb bytes.Buffer
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "exit 7"},
		Workdir: t.TempDir(),
	}, &out, &errb)
	require.NoError(t, err) // ran fine; non-zero exit is reported via code, not err
	assert.Equal(t, code, 7)
}

// COVER: Run's real run path (cmd.Run via exec.CommandContext) isn't
// fakeable through the container's exec/execOut seams, and reproducing the
// ctx-cancel race that makes os/exec wrap the *exec.ExitError isn't reliable
// to trigger from a test. so this exercises asExitError directly: a bare
// `err.(*exec.ExitError)` type assertion (the pre-fix code) panics on a
// wrapped ExitError; errors.As must not.
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
	_, err := New("bwrap", "")
	assert.Error(t, err, "not implemented")

	_, err = New("docker", "")
	assert.Error(t, err, "needs an image")

	_, err = New("nonsense", "x")
	assert.Error(t, err, "unknown driver")
}

func TestSortedEnvStable(t *testing.T) {
	got := sortedEnv(map[string]string{"B": "2", "A": "1", "C": "3"})
	assert.EqualArrays(t, got, []string{"A=1", "B=2", "C=3"})
}
