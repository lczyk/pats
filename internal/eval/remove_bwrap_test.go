//go:build !nobwrap

package eval

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox"
)

// a bwrap target is this machine's store: a rootfs a pin moved off goes like
// any other image, and one listed under another machine's name stays.
func TestRemoveSupersededBwrap(t *testing.T) {
	ctx := context.Background()
	host, err := os.Hostname()
	require.NoError(t, err)
	here := sandbox.BwrapDriver + ":" + host
	d, err := sandbox.Open(sandbox.Options{Driver: sandbox.BwrapDriver})
	require.NoError(t, err)

	id := "sha256:" + strings.Repeat("ab", 32)
	cache, err := sandbox.CacheDir()
	require.NoError(t, err)
	rootfs := filepath.Join(cache, "bwrap", "rootfs", strings.Replace(id, ":", "-", 1))
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	t.Cleanup(func() { os.RemoveAll(rootfs) })
	held, err := d.Images(ctx)
	require.NoError(t, err)
	require.That(t, held[id], "the store does not list a rootfs at %s", rootfs)

	// the list is a file anyone can edit: an id that is no image id names
	// nothing in the store, and nothing beside it.
	beside := filepath.Join(cache, "bwrap", "beside")
	require.NoError(t, os.MkdirAll(filepath.Join(beside, "kept"), 0o755))
	t.Cleanup(func() { os.RemoveAll(beside) })

	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}, Superseded: []superseded{
		{Target: here, ID: id, Pulled: true},
		{Target: sandbox.BwrapDriver + ":another-machine", ID: id, Pulled: true},
		{Target: here, ID: "../beside", Pulled: true},
		{Target: here, ID: "", Pulled: true},
	}}
	var out bytes.Buffer
	removeSuperseded(ctx, f, t.TempDir(), nil, time.Now(), logw{&out, false, 0})
	require.Len(t, f.Superseded, 1, out.String())
	assert.Equal(t, f.Superseded[0].Target, sandbox.BwrapDriver+":another-machine")
	held, err = d.Images(ctx)
	require.NoError(t, err)
	assert.That(t, !held[id], "the superseded rootfs is still in the store")
	_, err = os.Stat(filepath.Join(beside, "kept"))
	assert.NoError(t, err, "a dir beside the store went with a superseded id")
}
