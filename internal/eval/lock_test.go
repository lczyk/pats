package eval

import (
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestLockConfigDir(t *testing.T) {
	dir := t.TempDir()

	unlock, err := lockConfigDir(dir)
	require.NoError(t, err)

	// second lock on the same dir must fail while the first is held.
	_, err = lockConfigDir(dir)
	assert.Error(t, err, assert.AnyError)

	unlock()

	// after release, locking again must work.
	unlock2, err := lockConfigDir(dir)
	require.NoError(t, err)
	unlock2()
}
