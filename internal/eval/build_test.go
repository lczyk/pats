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

	"github.com/lczyk/pats/images"
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
		pins string
	}
	var lastUpstream map[string]string
	run := func(t *testing.T, sb config.Sandbox, fail bool) ([]call, []string, string, string, bool, error) {
		var calls []call
		h := &buildingHost{fakeHost: newFakeHost(), build: func(spec sandbox.BuildSpec, log io.Writer) (sandbox.Image, string, bool, error) {
			pins := ""
			if spec.Material != "" {
				b, _ := os.ReadFile(filepath.Join(spec.Material, "harness-versions"))
				pins = string(b)
			}
			calls = append(calls, call{spec.Base, spec.Argv, pins})
			spec.Progress("building " + spec.Base)
			spec.Warn("low on space")
			fmt.Fprintf(log, "build output of %s\n", spec.Base)
			if fail && len(calls) == 2 {
				return sandbox.Image{}, "", false, errors.New("provisioning exited 1")
			}
			n := strconv.Itoa(len(calls))
			return sandbox.Image{ID: "fp-" + n, Name: "pats-img-" + n}, "fp-" + spec.Base, len(calls) == 1, nil
		}}
		var out bytes.Buffer
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "provision.sh"), []byte("true\n"), 0o755))
		logPath := filepath.Join(t.TempDir(), "init", sb.Name+".log")
		_, layers, built, upstream, err := lxdImage(context.Background(), h, sb, Options{ConfigDir: dir, Out: &out, Verbose: 1}, logPath, false)
		b, _ := os.ReadFile(logPath)
		lastUpstream = upstream
		return calls, layers, out.String(), string(b), built, err
	}

	t.Run("the pats sandbox, then the provision", func(t *testing.T) {
		calls, layers, out, log, built, err := run(t, config.Sandbox{Name: "box", Driver: "lxd", Provision: "provision.sh"}, false)
		require.NoError(t, err)
		require.Equal(t, len(calls), 2)
		assert.EqualArrays(t, layers, []string{"fp-1"})
		// the base the bottom layer keyed on, for the upstream check.
		assert.EqualMaps(t, lastUpstream, map[string]string{lxdSandboxBase: "fp-" + lxdSandboxBase})
		assert.Equal(t, calls[0].base, lxdSandboxBase)
		assert.Equal(t, calls[1].base, "pats-img-1")
		// the pats layer carries the pins beside its script (and so in its key);
		// the project's layer does not.
		assert.Equal(t, calls[0].pins, images.HarnessVersions)
		assert.Equal(t, calls[1].pins, "")
		assert.That(t, len(calls[1].argv) > 0, "the provision step has no argv")
		assert.That(t, built, "the first build ran, but built is false")
		assert.ContainsString(t, out, `sandbox "box": building `+lxdSandboxBase)
		assert.ContainsString(t, out, `sandbox "box": low on space`)
		assert.ContainsString(t, log, "build output of "+lxdSandboxBase)
		assert.ContainsString(t, log, "build output of pats-img-1")
	})
	t.Run("a base of its own", func(t *testing.T) {
		calls, layers, _, _, built, err := run(t, config.Sandbox{Name: "box", Driver: "lxd", Base: "ubuntu:24.04"}, false)
		require.NoError(t, err)
		require.Equal(t, len(calls), 1)
		assert.Len(t, layers, 0)
		assert.EqualMaps(t, lastUpstream, map[string]string{"ubuntu:24.04": "fp-ubuntu:24.04"})
		assert.Equal(t, calls[0].base, "ubuntu:24.04")
		assert.That(t, built, "built is false")
	})
	t.Run("a failed build", func(t *testing.T) {
		_, _, _, _, _, err := run(t, config.Sandbox{Name: "box", Driver: "lxd", Provision: "provision.sh"}, true)
		require.Error(t, err, "provisioning exited 1")
		assert.ContainsString(t, err.Error(), "build output of pats-img-1")
		assert.ContainsString(t, err.Error(), "(full log: ")
		assert.ContainsString(t, err.Error(), filepath.Join("init", "box.log"))
	})
	t.Run("the pats sandbox alone", func(t *testing.T) {
		calls, layers, _, _, _, err := run(t, config.Sandbox{Name: "box", Driver: "lxd"}, false)
		require.NoError(t, err)
		require.Equal(t, len(calls), 1) // no empty project layer on top
		assert.Len(t, layers, 0)
	})
}

// buildingHost builds with build.
type buildingHost struct {
	*fakeHost
	build func(spec sandbox.BuildSpec, log io.Writer) (sandbox.Image, string, bool, error)
}

func (h *buildingHost) Build(_ context.Context, spec sandbox.BuildSpec, log io.Writer) (sandbox.Image, string, bool, error) {
	return h.build(spec, log)
}
