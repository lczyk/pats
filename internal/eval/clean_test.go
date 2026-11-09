package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// cleanDriver is a place in memory: its runs, and what clean removed.
type cleanDriver struct {
	sandbox.Driver
	runs    []sandbox.RunRecord
	err     error
	removed []string
}

func (d *cleanDriver) Runs(context.Context) ([]sandbox.RunRecord, error) { return d.runs, d.err }

func (d *cleanDriver) Target(context.Context) (sandbox.Target, error) {
	return sandbox.Target{Key: "docker:default", Daemon: "d1"}, nil
}

func (d *cleanDriver) Remove(_ context.Context, r sandbox.Resource) error {
	d.removed = append(d.removed, r.Kind+" "+r.Name)
	return nil
}

// clean judges every place's runs by their leases: a live run stays, forced
// or not, unless named; a dead one goes, sandboxes before networks; one this
// host cannot judge stays unless forced. a place that cannot be listed is
// skipped, and lease files stay while one is.
func TestCleanAcrossPlaces(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	root, err := sandbox.TempRoot()
	require.NoError(t, err)
	dir := filepath.Dir(root)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	host, _ := os.Hostname()
	lease := func(token string, at time.Time) string {
		p := filepath.Join(dir, "run-"+token+".lease")
		require.NoError(t, os.WriteFile(p, fmt.Appendf(nil, "%s %d\n", host, os.Getpid()), 0o644))
		require.NoError(t, os.Chtimes(p, at, at))
		return p
	}
	now := time.Now()
	lease("aaaaaaaaaaaa", now)
	deadLease := lease("bbbbbbbbbbbb", now.Add(-time.Hour))
	orphan := lease("cccccccccccc", now.Add(-time.Hour))
	old := now.Add(-time.Hour)
	run := func(token, owner string) sandbox.RunRecord {
		return sandbox.RunRecord{Token: token, Owner: owner, Resources: []sandbox.Resource{
			{Kind: "network", Name: "net-" + token, Created: old}, {Kind: "sandbox", Name: "ctr-" + token, Created: old}}}
	}
	places := func() (*cleanDriver, *cleanDriver) {
		return &cleanDriver{runs: []sandbox.RunRecord{run("aaaaaaaaaaaa", "x"), run("bbbbbbbbbbbb", "x"), run("dddddddddddd", "another/0/x")}},
			&cleanDriver{err: errors.New("Cannot connect to the Docker daemon")}
	}
	open := func(t *testing.T, ok, down *cleanDriver) {
		old := openDriver
		openDriver = func(o sandbox.Options) (sandbox.Driver, error) {
			if o.Driver == "podman" {
				return down, nil
			}
			return ok, nil
		}
		t.Cleanup(func() { openDriver = old })
	}

	ok, down := places()
	open(t, ok, down)
	var out bytes.Buffer
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker", "podman"}, LXDRemotes: []string{}, Out: &out}))
	assert.EqualArrays(t, ok.removed, []string{"sandbox ctr-bbbbbbbbbbbb", "network net-bbbbbbbbbbbb"})
	assert.ContainsString(t, out.String(), "WARN podman: skipped (Cannot connect to the Docker daemon)")
	assert.ContainsString(t, out.String(), "INFO docker: left 1 live run(s)")
	assert.ContainsString(t, out.String(), "docker: left 1 run(s) this clean cannot judge")
	_, err = os.Stat(deadLease)
	assert.That(t, os.IsNotExist(err), "a cleaned run's lease stayed")
	_, err = os.Stat(orphan)
	assert.NoError(t, err) // podman could not be listed: it may hold the run

	ok, down = places()
	open(t, ok, down)
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker"}, LXDRemotes: []string{}, All: true, Out: &out}))
	assert.EqualArrays(t, ok.removed, []string{"sandbox ctr-bbbbbbbbbbbb", "network net-bbbbbbbbbbbb", "sandbox ctr-dddddddddddd", "network net-dddddddddddd"})
	assert.ContainsString(t, out.String(), "WARN docker: run dddddddddddd (another/0/x) may be alive elsewhere")
	assert.ContainsString(t, out.String(), "INFO removed docker sandbox ctr-dddddddddddd (run dddddddddddd: another user's or machine's)")
	_, err = os.Stat(orphan)
	assert.That(t, os.IsNotExist(err), "an orphan stale lease stayed")

	ok, down = places()
	open(t, ok, down)
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker"}, LXDRemotes: []string{}, Run: "aaaaaaaaaaaa", Out: &out}))
	assert.EqualArrays(t, ok.removed, []string{"sandbox ctr-aaaaaaaaaaaa", "network net-aaaaaaaaaaaa"})
	assert.That(t, !strings.Contains(out.String(), "bbbbbbbbbbbb"), "--run took another run:\n%s", out.String())
}

// clean --all also takes what a project's pins say pats pulled or built on
// the daemon answering, an image: ref pulled by pats included; never what a
// pin names that pats did not make, or another daemon's.
func TestPinnedHere(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	proj := t.TempDir()
	_, err := registerProject(proj)
	require.NoError(t, err)
	require.NoError(t, updatePins(proj, func(f *pinFile) error {
		f.Targets["docker:default"] = map[string]*pinEntry{
			"pulled": {Daemon: "d1", Image: pinImage{ID: "sha256:aaa", Name: "ubuntu:24.04", Pulled: true}},
			"theirs": {Daemon: "d1", Image: pinImage{ID: "sha256:bbb", Name: "mine:1"}},
			"other":  {Daemon: "d2", Image: pinImage{ID: "sha256:ccc", Built: true}},
		}
		f.Superseded = []superseded{{Target: "docker:default", Daemon: "d1", ID: "sha256:ddd", Built: true}}
		return nil
	}))
	var got []string
	for _, r := range pinnedHere(context.Background(), &cleanDriver{}) {
		got = append(got, r.Name+" "+r.Label)
	}
	sort.Strings(got)
	assert.EqualArrays(t, got, []string{"sha256:aaa ubuntu:24.04", "sha256:ddd " + short("sha256:ddd")})
}

// clean reads this host's client id and never makes or repairs one, dry run
// or not, though judging a run compares owners.
func TestCleanWritesNoClientID(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	idPath := filepath.Join(cache, "pats", "client-id")
	old := openDriver
	openDriver = func(o sandbox.Options) (sandbox.Driver, error) {
		// a lease-less run: judging it compares its owner with this host's.
		run, owner := strings.Repeat("e", 12), "h/1/"+strings.Repeat("0", 16)
		return &cleanDriver{runs: []sandbox.RunRecord{{Token: run, Owner: owner,
			Resources: []sandbox.Resource{{Kind: "sandbox", Name: "c", Created: time.Now()}}}}}, nil
	}
	t.Cleanup(func() { openDriver = old })
	for _, malformed := range []bool{false, true} {
		if malformed {
			require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o755))
			require.NoError(t, os.WriteFile(idPath, []byte("not-an-id\n"), 0o600))
		}
		for _, dry := range []bool{true, false} {
			var out bytes.Buffer
			require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker"}, LXDRemotes: []string{}, DryRun: dry, All: true, Out: &out}))
			b, err := os.ReadFile(idPath)
			if malformed {
				require.NoError(t, err)
				assert.Equal(t, string(b), "not-an-id\n", "clean repaired the id")
			} else {
				assert.That(t, os.IsNotExist(err), "clean (dry run %v) made a client id", dry)
			}
			assert.ContainsString(t, out.String(), "may be alive elsewhere") // judged as another's
		}
	}
}
