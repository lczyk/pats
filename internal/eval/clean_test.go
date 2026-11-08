package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	assert.ContainsString(t, out.String(), "podman: skipped (Cannot connect to the Docker daemon)")
	assert.ContainsString(t, out.String(), "docker: left 1 live run(s)")
	assert.ContainsString(t, out.String(), "docker: left 1 run(s) this clean cannot judge")
	_, err = os.Stat(deadLease)
	assert.That(t, os.IsNotExist(err), "a cleaned run's lease stayed")
	_, err = os.Stat(orphan)
	assert.NoError(t, err) // podman could not be listed: it may hold the run

	ok, down = places()
	open(t, ok, down)
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker"}, LXDRemotes: []string{}, Force: true, Out: &out}))
	assert.EqualArrays(t, ok.removed, []string{"sandbox ctr-bbbbbbbbbbbb", "network net-bbbbbbbbbbbb", "sandbox ctr-dddddddddddd", "network net-dddddddddddd"})
	assert.ContainsString(t, out.String(), "may be alive elsewhere")
	_, err = os.Stat(orphan)
	assert.That(t, os.IsNotExist(err), "an orphan stale lease stayed")

	ok, down = places()
	open(t, ok, down)
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{"docker"}, LXDRemotes: []string{}, Run: "aaaaaaaaaaaa", Out: &out}))
	assert.EqualArrays(t, ok.removed, []string{"sandbox ctr-aaaaaaaaaaaa", "network net-aaaaaaaaaaaa"})
	assert.That(t, !strings.Contains(out.String(), "bbbbbbbbbbbb"), "--run took another run:\n%s", out.String())
}
