package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// deadPid is a pid that was a process and is gone.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	require.NoError(t, c.Run())
	return c.Process.Pid
}

// leaseOf writes token's lease (host, pid) and, unless ident is "none",
// its identity file, in p's home.
func leaseOf(t *testing.T, p *process, token, host string, pid int, ident string) string {
	t.Helper()
	path, err := p.leasePath(token)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(strings.TrimSuffix(path, "/"+leasePrefix+token+leaseSuffix), 0o755))
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, "%s %d\n", host, pid), 0o644))
	if ident != "none" {
		require.NoError(t, os.WriteFile(identPath(path), []byte(ident+"\n"), 0o644))
	}
	return path
}

// a run of this host, boot and namespace whose pid is gone loses its
// instance, bridge and acl; its marker, lease and identity stay.
func TestSweep(t *testing.T) {
	if identity() == "" {
		t.Skip("no identity on this os")
	}
	host, _ := os.Hostname()
	for name, c := range map[string]struct {
		host  string
		pid   func(t *testing.T) int
		ident string
		swept bool
	}{
		"dead":         {host, deadPid, identity(), true},
		"alive":        {host, func(*testing.T) int { return os.Getpid() }, identity(), false},
		"no identity":  {host, deadPid, "none", false},
		"another boot": {host, deadPid, "other-boot " + pidNS(), false},
		"another ns":   {host, deadPid, bootID() + " pid:[1]", false},
		"another host": {"elsewhere", deadPid, identity(), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			dead := lxdOn(t, f, Options{})
			sess, err := dead.Start(context.Background(), Spec{Workdir: t.TempDir()})
			require.NoError(t, err)
			s := sess.(*lxdSession)
			sweeper := lxdOn(t, f, Options{Remote: "srv"})
			sweeper.p.home = dead.p.home
			lease := leaseOf(t, sweeper.p, dead.p.token, c.host, c.pid(t), c.ident)
			var events []CleanEvent
			require.NoError(t, sweeper.p.sweep(context.Background(), sweeper, SweepOptions{Report: func(e CleanEvent) { events = append(events, e) }}))
			assert.Equal(t, !f.has("instance", s.name), c.swept)
			assert.Equal(t, !f.has("network", s.bridge), c.swept)
			assert.Equal(t, !f.has("acl", s.bridge), c.swept)
			assert.That(t, f.has("acl", dead.marker()), "the marker went")
			_, err = os.Stat(lease)
			assert.NoError(t, err)
			if c.swept {
				assert.Equal(t, len(events), 3)
				assert.Equal(t, events[0].Kind, "sandbox") // first: it holds its network
			} else {
				assert.Equal(t, len(events), 0)
			}
		})
	}
}

// a dry run says what it would stop and stops nothing.
func TestSweepDryRun(t *testing.T) {
	if identity() == "" {
		t.Skip("no identity on this os")
	}
	host, _ := os.Hostname()
	f := newFakeServer(t)
	dead := lxdOn(t, f, Options{})
	sess, err := dead.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	s := sess.(*lxdSession)
	sweeper := lxdOn(t, f, Options{Remote: "srv"})
	sweeper.p.home = dead.p.home
	leaseOf(t, sweeper.p, dead.p.token, host, deadPid(t), identity())
	var events []CleanEvent
	require.NoError(t, sweeper.p.sweep(context.Background(), sweeper, SweepOptions{DryRun: true, Report: func(e CleanEvent) { events = append(events, e) }}))
	assert.That(t, f.has("instance", s.name), "a dry run removed")
	assert.Equal(t, len(events), 3)
	assert.That(t, events[0].DryRun, "not marked dry")
}

// the lease's first write brings its identity; its end takes both.
func TestLeaseIdent(t *testing.T) {
	if identity() == "" {
		t.Skip("no identity on this os")
	}
	p := testProcess(t)
	require.NoError(t, p.holdLease())
	path, _ := p.leasePath(p.token)
	id, err := readIdent(path)
	require.NoError(t, err)
	assert.Equal(t, id, identity())
	// an older pats's reading of the lease is as before.
	l, ok, err := readLeaseAt(path)
	require.NoError(t, err)
	assert.That(t, ok && l.pid == os.Getpid(), "lease read: %+v", l)
	p.endLease()
	_, err = os.Stat(identPath(path))
	assert.That(t, os.IsNotExist(err), "identity left after the lease")
}

// clean takes a dead run's identity with its lease, in one event; an
// identity with no lease goes once old, and is listed once in a dry run.
func TestCleanIdent(t *testing.T) {
	host, _ := os.Hostname()
	p := testProcess(t)
	old := time.Now().Add(-48 * time.Hour)
	withLease := leaseOf(t, p, "aaaaaaaaaaaa", host, deadPid(t), "b -")
	require.NoError(t, os.Chtimes(withLease, old, old))
	orphan := leaseOf(t, p, "bbbbbbbbbbbb", host, 1, "b -")
	require.NoError(t, os.Remove(orphan))
	require.NoError(t, os.Chtimes(identPath(orphan), old, old))
	young := leaseOf(t, p, "cccccccccccc", host, 1, "b -")
	require.NoError(t, os.Remove(young))

	var dry bytes.Buffer
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, true, false, TextReport(&dry)))
	assert.ContainsString(t, dry.String(), "would remove lease "+withLease+" and its identity file")
	assert.Equal(t, strings.Count(dry.String(), "identity file "+identPath(orphan)), 1)
	assert.That(t, !strings.Contains(dry.String(), identPath(withLease)+"\n"), "the lease's identity listed twice")

	var out bytes.Buffer
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, false, false, TextReport(&out)))
	for _, gone := range []string{withLease, identPath(withLease), identPath(orphan)} {
		_, err := os.Stat(gone)
		assert.That(t, os.IsNotExist(err), "%s left", gone)
	}
	_, err := os.Stat(identPath(young))
	assert.NoError(t, err)
}

// fakeRuns is a driver with only what a sweep calls.
type fakeRuns struct {
	Driver
	runs    []RunRecord
	failOn  string
	removed []string
}

func (f *fakeRuns) Runs(context.Context) ([]RunRecord, error) { return f.runs, nil }

func (f *fakeRuns) Remove(_ context.Context, r Resource) error {
	if r.Name == f.failOn {
		return fmt.Errorf("remove %s: busy", r.Name)
	}
	f.removed = append(f.removed, r.Kind+" "+r.Name)
	return nil
}

// a removal that fails stops that run's sweep where it is: what comes
// after may be held by it. kinds a sweep does not take are left.
func TestSweepRemoveFails(t *testing.T) {
	if identity() == "" {
		t.Skip("no identity on this os")
	}
	host, _ := os.Hostname()
	p := testProcess(t)
	leaseOf(t, p, "aaaaaaaaaaaa", host, deadPid(t), identity())
	d := &fakeRuns{failOn: "box", runs: []RunRecord{{Token: runA, Resources: []Resource{
		{Kind: "image", Name: "img"}, {Kind: "acl", Name: "acl"}, {Kind: "network", Name: "net"},
		{Kind: "sandbox", Name: "box"}, {Kind: "marker", Name: "pats-run-aaaaaaaaaaaa"}, {Kind: "alias", Name: "al"},
	}}}}
	err := p.sweep(context.Background(), d, SweepOptions{})
	require.Error(t, err, "busy")
	assert.Equal(t, len(d.removed), 0)
	d.failOn = ""
	require.NoError(t, p.sweep(context.Background(), d, SweepOptions{}))
	assert.Equal(t, strings.Join(d.removed, ","), "sandbox box,acl acl,network net")
}

// an identity whose lease is gone, or a lease only stale, is never swept.
func TestSweepNeedsBoth(t *testing.T) {
	if identity() == "" {
		t.Skip("no identity on this os")
	}
	host, _ := os.Hostname()
	p := testProcess(t)
	path := leaseOf(t, p, "aaaaaaaaaaaa", host, deadPid(t), identity())
	require.NoError(t, os.Remove(path))
	leaseOf(t, p, "bbbbbbbbbbbb", host, os.Getpid(), identity())
	stale := time.Now().Add(-time.Hour)
	bpath, _ := p.leasePath("bbbbbbbbbbbb")
	require.NoError(t, os.Chtimes(bpath, stale, stale))
	d := &fakeRuns{runs: []RunRecord{
		{Token: runA, Resources: []Resource{{Kind: "sandbox", Name: "a"}}},
		{Token: runB, Resources: []Resource{{Kind: "sandbox", Name: "b"}}},
	}}
	require.NoError(t, p.sweep(context.Background(), d, SweepOptions{}))
	assert.Equal(t, len(d.removed), 0)
}

// two run tokens for the fakes.
var runA, runB = strings.Repeat("a", 12), strings.Repeat("b", 12)
