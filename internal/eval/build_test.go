package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
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
