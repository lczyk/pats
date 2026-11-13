package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the docker snap's cli reads a build context under the home, a hidden dir
// below its top included, and none outside it, in a hidden dir at its top or
// in another snap's data.
func TestDockerSnapReach(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	for _, d := range []string{"home/proj/.build", "home/.work/ctx", "home/snap/other/ctx", "home/snap/docker/common/ctx", "srv/ctx"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	require.NoError(t, os.Symlink(filepath.Join(base, "srv", "ctx"), filepath.Join(home, "proj", "linked")))

	for _, ok := range []string{"proj", "proj/.build", "snap/docker/common/ctx", "proj/not-made-yet"} {
		assert.NoError(t, dockerSnapReach(filepath.Join(home, ok), home))
	}
	for dir, why := range map[string]string{
		filepath.Join(base, "srv", "ctx"):        "not under the home",
		filepath.Join(home, "proj", "linked"):    "not under the home",
		filepath.Join(home, ".work", "ctx"):      "in a hidden directory at the top of the home",
		filepath.Join(home, "snap", "other"):     "in another snap's data",
		filepath.Join(home, "snap", "other/ctx"): "in another snap's data",
		home:                                     "",
	} {
		err := dockerSnapReach(dir, home)
		if why == "" {
			assert.NoError(t, err)
			continue
		}
		assert.Error(t, err, why)
		assert.ContainsString(t, err.Error(), dir)
	}
	assert.Error(t, dockerSnapReach(filepath.Join(home, "proj"), ""), "not under the home")
}
