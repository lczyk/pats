package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestRunPrivateAlias(t *testing.T) {
	assert.That(t, runPrivateAlias("pats-img-0123456789abcdef01234567-6be5c5b2da33"), "a run-private alias not seen")
	assert.That(t, !runPrivateAlias("pats-img-0123456789abcdef01234567"), "a content alias read as run-private")
	assert.That(t, !runPrivateAlias("pats-img-mine-x"), "a hand-made alias read as run-private")
}

// an image's use reads back as times, a never-used one as zero, and a
// run-private alias on it is seen.
func TestLXDImageUsageOnServer(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	fp, _, err := l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	up := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.images[fp].uploaded = up
	u, err := l.imageUsage(context.Background(), fp)
	require.NoError(t, err)
	assert.That(t, u.LastUsed.IsZero(), "never used, but last used %v", u.LastUsed)
	assert.That(t, u.Uploaded.Equal(up), "uploaded %v", u.Uploaded)
	assert.That(t, !u.RunPrivate, "no alias, yet run-private")

	f.images[fp].lastUsed = up.Add(time.Hour)
	f.images[fp].aliases = []string{"pats-img-0123456789abcdef01234567-" + l.p.token}
	u, err = l.imageUsage(context.Background(), fp)
	require.NoError(t, err)
	assert.That(t, u.LastUsed.Equal(up.Add(time.Hour)), "last used %v", u.LastUsed)
	assert.That(t, u.RunPrivate, "a run-private alias not seen")
}
