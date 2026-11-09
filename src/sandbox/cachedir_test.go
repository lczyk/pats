package sandbox

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
)

// one cache dir on every os: an absolute XDG_CACHE_HOME's pats, else
// ~/.cache/pats; a relative XDG_CACHE_HOME is ignored.
func TestCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, c := range []struct{ xdg, want string }{
		{"", filepath.Join(home, ".cache", "pats")},
		{"/abs/cache", "/abs/cache/pats"},
		{"rel/cache", filepath.Join(home, ".cache", "pats")},
	} {
		t.Setenv("XDG_CACHE_HOME", c.xdg)
		got, err := CacheDir()
		require.NoError(t, err)
		assert.Equal(t, got, c.want, c.xdg)
	}
}

// the legacy dir is none when it is the cache dir by another spelling.
func TestLegacyCacheDirSame(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cur, err := os.UserCacheDir()
	require.NoError(t, err)
	t.Setenv("XDG_CACHE_HOME", cur) // CacheDir is now os.UserCacheDir()/pats
	assert.Equal(t, legacyCacheDir(), "")
	link := filepath.Join(home, "link")
	require.NoError(t, os.MkdirAll(cur, 0o755))
	require.NoError(t, os.Symlink(cur, link))
	t.Setenv("XDG_CACHE_HOME", link)
	assert.Equal(t, legacyCacheDir(), "")
}

// the client id moves over from the legacy dir once, under the new dir's
// lock; a valid id in the new dir is kept; a malformed legacy one is not
// carried.
func TestClientIDMigrates(t *testing.T) {
	dir, legacy := t.TempDir(), t.TempDir()
	write := func(p, id string) { require.NoError(t, os.WriteFile(p, []byte(id+"\n"), 0o600)) }
	fromLegacy := func() string {
		if id := readClientID(filepath.Join(legacy, "client-id")); id != "" {
			return id
		}
		return "00000000000000aa"
	}
	write(filepath.Join(legacy, "client-id"), "1111111111111111")
	id, err := loadClientID(filepath.Join(dir, "client-id"), fromLegacy)
	require.NoError(t, err)
	assert.Equal(t, id, "1111111111111111")
	assert.Equal(t, readClientID(filepath.Join(dir, "client-id")), "1111111111111111")

	write(filepath.Join(legacy, "client-id"), "2222222222222222")
	id, err = loadClientID(filepath.Join(dir, "client-id"), fromLegacy)
	require.NoError(t, err)
	assert.Equal(t, id, "1111111111111111") // the new dir's stands

	dir2 := t.TempDir()
	write(filepath.Join(legacy, "client-id"), "not-an-id")
	id, err = loadClientID(filepath.Join(dir2, "client-id"), fromLegacy)
	require.NoError(t, err)
	assert.Equal(t, id, "00000000000000aa")
}

// clean finds a run's lease in the legacy dir: a live older pats's run is
// alive, not judged by its age; a dead one's legacy lease goes with it.
func TestLegacyLease(t *testing.T) {
	p := testProcess(t)
	legacy := t.TempDir()
	p.legacy = func() string { return legacy }
	p.owner = func() string { return "me" }
	host, _ := os.Hostname()
	now := time.Now()
	lease := filepath.Join(legacy, "run-0123456789ab.lease")
	require.NoError(t, os.WriteFile(lease, []byte(host+" "+itoa(os.Getpid())+"\n"), 0o644))
	old := now.Add(-time.Hour)
	run := "0123456789ab"
	v, _ := p.judge(RunRecord{Token: run, Owner: "me", Resources: []Resource{{Kind: "sandbox", Created: old}}}, now)
	assert.Equal(t, v, alive)

	require.NoError(t, os.Chtimes(lease, old, old))
	d := &memDriver{runs: []RunRecord{{Token: run, Owner: "me", Resources: []Resource{{Kind: "sandbox", Name: "c", Created: old}}}}}
	var out bytes.Buffer
	_, err := p.clean(t.Context(), d, CleanOptions{Place: "docker", Report: TextReport(&out)})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), "removed lease "+lease)
	_, err = os.Stat(lease)
	assert.That(t, os.IsNotExist(err), "the legacy lease stayed")
}

// the legacy dir's temp root is swept by the same rules; once only the old
// client id is left, clean says so and removes nothing of it; a dry run
// says the same.
func TestCleanTempLegacy(t *testing.T) {
	p := testProcess(t)
	legacy := t.TempDir()
	p.legacy = func() string { return legacy }
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "client-id"), []byte("1111111111111111\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "client-id.lock"), nil, 0o600))
	dead := filepath.Join(legacy, "tmp", "pats-work-1")
	require.NoError(t, os.MkdirAll(dead, 0o700))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(dead, old, old))

	var dry, real bytes.Buffer
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, true, false, TextReport(&dry)))
	_, err := os.Stat(dead)
	require.NoError(t, err)
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, false, false, TextReport(&real)))
	assert.Equal(t, real.String(), "removed temp dir "+dead+"\n"+
		"left legacy cache dir "+legacy+": it holds only an old client id; remove it once no older pats is installed\n")
	assert.Equal(t, dry.String(), "would remove temp dir "+dead+"\n"+
		"left legacy cache dir "+legacy+": it holds only an old client id; remove it once no older pats is installed\n")
	for _, f := range []string{"client-id", "client-id.lock", "tmp"} {
		_, err := os.Stat(filepath.Join(legacy, f))
		assert.NoError(t, err, f)
	}

	// a lease the run sweep removes, or would in a dry run, counts as gone.
	lease := filepath.Join(legacy, "run-0123456789ab.lease")
	require.NoError(t, os.WriteFile(lease, []byte("h 1\n"), 0o644))
	dry.Reset()
	require.NoError(t, p.cleanTemp(map[string]bool{"0123456789ab": true}, map[string]bool{lease: true}, 24*time.Hour, true, false, TextReport(&dry)))
	assert.ContainsString(t, dry.String(), "left legacy cache dir")
	dry.Reset()
	require.NoError(t, p.cleanTemp(map[string]bool{"0123456789ab": true}, nil, 24*time.Hour, true, false, TextReport(&dry)))
	assert.That(t, !strings.Contains(dry.String(), "left legacy cache dir"), "a lease nothing removes is not gone")
}

// memDriver is a place in memory for clean: its runs, and what was removed.
type memDriver struct {
	Driver
	runs    []RunRecord
	removed []string
}

func (d *memDriver) Runs(context.Context) ([]RunRecord, error) { return d.runs, nil }

func (d *memDriver) Remove(_ context.Context, r Resource) error {
	d.removed = append(d.removed, r.Kind+" "+r.Name)
	return nil
}

// a legacy lease that cannot be looked at is not an absent one: the run is
// unknown, never judged dead by its age.
func TestLegacyLeaseUnreadable(t *testing.T) {
	p := testProcess(t)
	legacy := filepath.Join(t.TempDir(), "legacy")
	require.NoError(t, os.MkdirAll(legacy, 0o755))
	p.legacy = func() string { return legacy }
	p.owner = func() string { return "me" }
	require.NoError(t, os.Chmod(legacy, 0))
	defer os.Chmod(legacy, 0o755)
	old := time.Now().Add(-time.Hour)
	run := strings.Repeat("ab", 6)
	v, _ := p.judge(RunRecord{Token: run, Owner: "me", Resources: []Resource{{Kind: "sandbox", Created: old}}}, time.Now())
	assert.Equal(t, v, unknown)
}
