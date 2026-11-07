package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

func TestResolveBuildSpec(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile.other"), []byte("FROM scratch\n"), 0o644))

	// a dir spec -> context is the dir, Dockerfile inside it.
	ctxDir, df, err := resolveBuildSpec(dir, ".")
	require.NoError(t, err)
	assert.Equal(t, ctxDir, dir)
	assert.Equal(t, df, filepath.Join(dir, "Dockerfile"))

	// a file spec -> context is its dir.
	ctxDir, df, err = resolveBuildSpec(dir, "Dockerfile.other")
	require.NoError(t, err)
	assert.Equal(t, ctxDir, dir)
	assert.Equal(t, df, filepath.Join(dir, "Dockerfile.other"))

	// missing spec -> error.
	_, _, err = resolveBuildSpec(dir, "nope")
	assert.Error(t, err, "build context")
}

// an lxd sandbox's image: with no base the pats sandbox is built first and
// the sandbox's own provision on top of it, keyed in a chain; each build's
// progress and warnings reach the run's output under the sandbox's id, its
// output the sandbox's build log, and a failure carries the log's tail and
// where the whole log is.
func TestLXDImageBuild(t *testing.T) {
	type call struct {
		base string
		argv []string
	}
	run := func(t *testing.T, sb config.Sandbox, fail bool) ([]call, string, string, bool, error) {
		var calls []call
		old := lxdImageFn
		lxdImageFn = func(ctx context.Context, o sandbox.Options, spec sandbox.LXDImageSpec, log io.Writer) (string, bool, error) {
			calls = append(calls, call{spec.Base, spec.Argv})
			assert.Equal(t, o.Driver, "lxd")
			spec.Progress("building " + spec.Base)
			spec.Warn("low on space")
			fmt.Fprintf(log, "build output of %s\n", spec.Base)
			if fail && len(calls) == 2 {
				return "", false, errors.New("provisioning exited 1")
			}
			return "pats-img-" + strconv.Itoa(len(calls)), len(calls) == 1, nil
		}
		defer func() { lxdImageFn = old }()
		var out bytes.Buffer
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "provision.sh"), []byte("true\n"), 0o755))
		run := t.TempDir()
		_, built, err := lxdImage(context.Background(), sb, Options{ConfigDir: dir, Out: &out}, run)
		b, _ := os.ReadFile(filepath.Join(run, "images", sb.ID+".log"))
		return calls, out.String(), string(b), built, err
	}

	t.Run("the pats sandbox, then the provision", func(t *testing.T) {
		calls, out, log, built, err := run(t, config.Sandbox{ID: "box", Driver: "lxd", Provision: "provision.sh"}, false)
		require.NoError(t, err)
		require.Equal(t, len(calls), 2)
		assert.Equal(t, calls[0].base, lxdSandboxBase)
		assert.Equal(t, calls[1].base, "pats-img-1")
		assert.That(t, len(calls[1].argv) > 0, "the provision step has no argv")
		assert.That(t, built, "the first build ran, but built is false")
		assert.ContainsString(t, out, `sandbox "box": building `+lxdSandboxBase)
		assert.ContainsString(t, out, `sandbox "box": low on space`)
		assert.ContainsString(t, log, "build output of "+lxdSandboxBase)
		assert.ContainsString(t, log, "build output of pats-img-1")
	})
	t.Run("a base of its own", func(t *testing.T) {
		calls, _, _, built, err := run(t, config.Sandbox{ID: "box", Driver: "lxd", Base: "ubuntu:24.04"}, false)
		require.NoError(t, err)
		require.Equal(t, len(calls), 1)
		assert.Equal(t, calls[0].base, "ubuntu:24.04")
		assert.That(t, built, "built is false")
	})
	t.Run("a failed build", func(t *testing.T) {
		_, _, _, _, err := run(t, config.Sandbox{ID: "box", Driver: "lxd", Provision: "provision.sh"}, true)
		require.Error(t, err, "provisioning exited 1")
		assert.ContainsString(t, err.Error(), "build output of pats-img-1")
		assert.ContainsString(t, err.Error(), "(full log: ")
		assert.ContainsString(t, err.Error(), filepath.Join("images", "box.log"))
	})
}
