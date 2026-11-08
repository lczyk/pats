package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the verdict on a run: by the lease the place holds (lxd), or by its lease
// file on this host; with no record, by age where this host could have
// read one, and unknown where it could not.
func TestJudge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := testProcess(t)
	p.owner = func() string { return "me" }
	old, young := now.Add(-time.Hour), now.Add(-time.Minute)
	lease := func(token, body string, at time.Time) {
		path, err := p.leasePath(token)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		require.NoError(t, os.Chtimes(path, at, at))
	}
	host, _ := os.Hostname()
	lease("aa01", host+" "+itoa(os.Getpid())+"\n", young)
	lease("aa02", host+" "+itoa(os.Getpid())+"\n", old)
	lease("aa03", host+" 999999999\n", young) // a pid that is gone
	lease("aa04", "elsewhere 999999999\n", young)
	res := func(at time.Time) []Resource { return []Resource{{Kind: "sandbox", Created: at}} }
	for _, c := range []struct {
		name string
		r    RunRecord
		want verdict
	}{
		{"held, fresh", RunRecord{Token: "x", Held: true, Lease: young, Resources: res(old)}, alive},
		{"held, stale", RunRecord{Token: "x", Held: true, Lease: old, Resources: res(young)}, dead},
		{"held, no marker, young", RunRecord{Token: "x", Held: true, Resources: res(young)}, alive},
		{"held, no marker, old", RunRecord{Token: "x", Held: true, Resources: res(old)}, dead},
		{"file, fresh", RunRecord{Token: "aa01", Owner: "other", Resources: res(old)}, alive},
		{"file, stale", RunRecord{Token: "aa02", Owner: "me", Resources: res(young)}, dead},
		{"file, its pid gone here", RunRecord{Token: "aa03", Owner: "me", Resources: res(young)}, dead},
		{"file, another host's pid", RunRecord{Token: "aa04", Owner: "me", Resources: res(old)}, alive},
		{"no file, mine, young", RunRecord{Token: "bb01", Owner: "me", Resources: res(young)}, alive},
		{"no file, mine, old", RunRecord{Token: "bb02", Owner: "me", Resources: res(old)}, dead},
		{"no file, another's", RunRecord{Token: "bb03", Owner: "other", Resources: res(old)}, unknown},
	} {
		got, why := p.judge(c.r, now)
		assert.Equal(t, got, c.want, "%s (%s)", c.name, why)
	}
}

func itoa(n int) string { return string(fmt.Appendf(nil, "%d", n)) }

// a run's lease file: made once, before anything of the run; renewed on the
// wall clock once leaseEvery has passed, at a wake; made again when a clean
// took it; gone at shutdown.
func TestHostLease(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	p := testProcess(t)
	mc := newManualClock(start)
	p.clock = mc
	p.t.leaseEvery, p.t.leaseWake = 2*time.Minute, 30*time.Second
	require.NoError(t, p.holdLease())
	path, _ := p.leasePath(p.token)
	at := func() time.Time {
		st, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return st.ModTime()
	}
	assert.Equal(t, at().Equal(start), true)
	waitFor := func(want time.Time) {
		deadline := time.Now().Add(5 * time.Second)
		for !at().Equal(want) {
			require.That(t, time.Now().Before(deadline), "the lease was not renewed to %s (at %s)", want, at())
			time.Sleep(time.Millisecond)
		}
	}
	for range 3 {
		mc.Advance(30 * time.Second)
	}
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, at().Equal(start), true) // not yet due
	mc.Advance(30 * time.Second)
	waitFor(start.Add(2 * time.Minute))
	require.NoError(t, os.Remove(path))
	for range 4 {
		mc.Advance(30 * time.Second)
	}
	waitFor(start.Add(4 * time.Minute))
	p.shutdown()
	_, err := os.Stat(path)
	assert.That(t, os.IsNotExist(err), "the lease outlived its run")
}

// temp dirs go once their run is dead and they are old; a lease file goes
// once stale and naming nothing listed and no temp dir, and stays while any
// place could not be listed.
func TestCleanTemp(t *testing.T) {
	now := time.Now()
	p := testProcess(t)
	run := testProcess(t)
	run.home = p.home
	dir, err := run.mkTemp("pats-work-")
	require.NoError(t, err)
	old := now.Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(dir, old, old))

	// a token of digits alone is a token, not an old pid file's pid.
	digits := testProcess(t)
	digits.home, digits.token = p.home, "000099999999"
	ddir, err := digits.mkTemp("pats-work-")
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(ddir, old, old))

	var out bytes.Buffer
	require.NoError(t, p.cleanTemp(map[string]bool{}, 24*time.Hour, false, &out))
	_, err = os.Stat(dir)
	assert.NoError(t, err) // its run is alive
	_, err = os.Stat(ddir)
	assert.NoError(t, err) // so is this one

	crashLease(run)
	home, _ := p.home()
	stale := now.Add(-time.Hour)
	leasePath, _ := run.leasePath(run.token)
	require.NoError(t, os.Chtimes(leasePath, stale, stale))
	orphan := filepath.Join(home, "run-0123456789ab.lease")
	require.NoError(t, os.WriteFile(orphan, []byte("elsewhere 1\n"), 0o644))
	require.NoError(t, os.Chtimes(orphan, stale, stale))
	listed := filepath.Join(home, "run-ffffffffffff.lease")
	require.NoError(t, os.WriteFile(listed, []byte("elsewhere 1\n"), 0o644))
	require.NoError(t, os.Chtimes(listed, stale, stale))

	require.NoError(t, p.cleanTemp(nil, 24*time.Hour, false, &out))
	_, err = os.Stat(orphan)
	assert.NoError(t, err) // a place was not listed: lease files stay
	_, err = os.Stat(dir)
	assert.That(t, os.IsNotExist(err), "a dead run's old temp dir stayed")

	require.NoError(t, p.cleanTemp(map[string]bool{"ffffffffffff": true}, 24*time.Hour, false, &out))
	_, err = os.Stat(orphan)
	assert.That(t, os.IsNotExist(err), "an orphan stale lease stayed")
	_, err = os.Stat(listed)
	assert.NoError(t, err) // a run a place still holds
}
