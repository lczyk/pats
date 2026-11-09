package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a file given as a cli's stdin, stdout or stderr reaches it as a pipe, never
// as the descriptor itself: a snap-confined cli cannot inherit a descriptor
// outside its confinement, and apparmor swaps it for /dev/null.
func TestExecRunnerPipesFiles(t *testing.T) {
	const probe = `for fd in 0 1 2; do if [ -p /dev/fd/$fd ]; then echo pipe; else echo file; fi; done >&2`
	open := func(t *testing.T) (in, out, errf *os.File) {
		dir := t.TempDir()
		var err error
		in, err = os.Create(filepath.Join(dir, "in"))
		require.NoError(t, err)
		out, err = os.Create(filepath.Join(dir, "out"))
		require.NoError(t, err)
		errf, err = os.Create(filepath.Join(dir, "err"))
		require.NoError(t, err)
		t.Cleanup(func() { in.Close(); out.Close(); errf.Close() })
		return in, out, errf
	}
	r := execRunner{bin: "sh"}
	t.Run("run", func(t *testing.T) {
		in, out, errf := open(t)
		require.NoError(t, r.Run(context.Background(), in, out, errf, "-c", probe))
		b, err := os.ReadFile(errf.Name())
		require.NoError(t, err)
		assert.Equal(t, string(b), "pipe\npipe\npipe\n")
	})
	t.Run("start", func(t *testing.T) {
		in, out, errf := open(t)
		x, err := r.Start(in, out, errf, "-c", probe)
		require.NoError(t, err)
		require.NoError(t, x.Wait())
		b, err := os.ReadFile(errf.Name())
		require.NoError(t, err)
		assert.Equal(t, string(b), "pipe\npipe\npipe\n")
	})
	t.Run("output still arrives", func(t *testing.T) {
		_, out, errf := open(t)
		var in bytes.Buffer
		in.WriteString("hello\n")
		require.NoError(t, r.Run(context.Background(), &in, out, errf, "-c", "cat; echo oops >&2"))
		b, _ := os.ReadFile(out.Name())
		assert.Equal(t, string(b), "hello\n")
		b, _ = os.ReadFile(errf.Name())
		assert.Equal(t, string(b), "oops\n")
	})
}
