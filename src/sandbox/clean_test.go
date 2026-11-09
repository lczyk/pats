package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	require.NoError(t, p.cleanTemp(map[string]bool{}, 24*time.Hour, false, false, TextReport(&out)))
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

	require.NoError(t, p.cleanTemp(nil, 24*time.Hour, false, false, TextReport(&out)))
	_, err = os.Stat(orphan)
	assert.NoError(t, err) // a place was not listed: lease files stay
	_, err = os.Stat(dir)
	assert.That(t, os.IsNotExist(err), "a dead run's old temp dir stayed")

	require.NoError(t, p.cleanTemp(map[string]bool{"ffffffffffff": true}, 24*time.Hour, false, false, TextReport(&out)))
	_, err = os.Stat(orphan)
	assert.That(t, os.IsNotExist(err), "an orphan stale lease stayed")
	_, err = os.Stat(listed)
	assert.NoError(t, err) // a run a place still holds

	// force takes a live run's dir and every lease, a dry run saying so first.
	out.Reset()
	require.NoError(t, p.cleanTemp(nil, 24*time.Hour, true, true, TextReport(&out)))
	assert.ContainsString(t, out.String(), "would remove temp dir "+ddir+"\n")
	assert.ContainsString(t, out.String(), "would remove lease "+listed+"\n")
	_, err = os.Stat(ddir)
	assert.NoError(t, err)
	require.NoError(t, p.cleanTemp(nil, 24*time.Hour, false, true, TextReport(&out)))
	for _, gone := range []string{ddir, listed} {
		_, err = os.Stat(gone)
		assert.That(t, os.IsNotExist(err), "force left %s", gone)
	}
}

// fakeImageDaemon answers clean's image calls from imgs, and records what
// it removes.
func fakeImageDaemon(t *testing.T, imgs []daemonImage, users map[string]string) (runner, *[]string) {
	var calls []string
	byID := map[string]daemonImage{}
	for _, im := range imgs {
		byID[im.ID] = im
	}
	return runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "ps ") && strings.Contains(joined, "ancestor="):
			io.WriteString(stdout, users[strings.TrimPrefix(args[len(args)-1], "ancestor=")])
		case strings.HasPrefix(joined, "ps ") || strings.HasPrefix(joined, "network ls"):
		case joined == "images -a -q --no-trunc":
			for _, im := range imgs {
				io.WriteString(stdout, im.ID+"\n")
			}
		case strings.HasPrefix(joined, "image inspect --format"):
			io.WriteString(stdout, strings.Join(byID[args[len(args)-1]].RepoTags, " ")+"\n")
		case strings.HasPrefix(joined, "image inspect"):
			var out []daemonImage
			for _, id := range args[2:] {
				out = append(out, byID[id])
			}
			b, _ := json.Marshal(out)
			stdout.Write(b)
		case args[0] == "rm" || args[0] == "rmi":
			calls = append(calls, joined)
		default:
			t.Fatalf("unexpected call: %s", joined)
		}
		return nil
	}), &calls
}

func image(id string, tags []string, label string, layers ...string) daemonImage {
	im := daemonImage{ID: id, RepoTags: tags}
	if label != "" {
		im.Config.Labels = map[string]string{hashLabel: label}
	}
	im.RootFS.Layers = layers
	return im
}

// clean --all takes every image pats made on a container daemon: built
// ones and those pulled from its registry, ones built on others first, by
// their tags so one under several goes. one a container uses stays, and so
// does one an image that stays is built on: someone's own image FROM a pats
// build, which inherits its label, is theirs. a dry run says exactly what
// the real one does.
func TestCleanCachedContainerImages(t *testing.T) {
	base := image("sha256:base", []string{sandboxRepo + ":v1", sandboxRepo + ":latest"}, "", "l1")
	parent := image("sha256:parent", nil, "h1", "l1", "l2")
	child := image("sha256:child", []string{"pats-sbx:a"}, "h2", "l1", "l2", "l3")
	used := image("sha256:used", []string{proxyRepo + ":v1"}, "", "p1")
	users := map[string]string{"sha256:used": "agent-1\n"}

	r, calls := fakeImageDaemon(t, []daemonImage{used, base, parent, child}, users)
	c := newContainer(testProcess(t), r, Options{Driver: "docker"})
	var dry, out bytes.Buffer
	_, err := testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", All: true, DryRun: true, Report: TextReport(&dry)})
	require.NoError(t, err)
	assert.Len(t, *calls, 0)
	_, err = testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", All: true, Report: TextReport(&out)})
	require.NoError(t, err)
	assert.EqualArrays(t, *calls, []string{"rmi pats-sbx:a", "rmi sha256:parent", "rmi " + sandboxRepo + ":v1 " + sandboxRepo + ":latest"})
	assert.Equal(t, out.String(), "docker: kept image "+proxyRepo+":v1 (used by agent-1)\n"+
		"removed docker image pats-sbx:a (cached)\n"+
		"removed docker image parent (cached)\n"+
		"removed docker image "+sandboxRepo+":v1 (cached)\n")
	assert.Equal(t, dry.String(), strings.ReplaceAll(out.String(), "removed", "would remove"))

	// --force takes the container using it first, then the image.
	*calls = nil
	out.Reset()
	_, err = testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", Force: true, Report: TextReport(&out)})
	require.NoError(t, err)
	assert.EqualArrays(t, *calls, []string{"rm -f agent-1", "rmi pats-sbx:a", "rmi sha256:parent",
		"rmi " + sandboxRepo + ":v1 " + sandboxRepo + ":latest", "rmi " + proxyRepo + ":v1"})
	assert.ContainsString(t, out.String(), "removed docker sandbox agent-1 (it uses image "+proxyRepo+":v1, forced)\n")

	// a pin naming an image the daemon no longer holds is not news: it
	// would say "removed" on every clean.
	*calls = nil
	out.Reset()
	_, err = testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", All: true, Report: TextReport(&out),
		Pinned: []Resource{{Kind: "image", Name: "sha256:gone", Label: "gone:1"}}})
	require.NoError(t, err)
	assert.That(t, !strings.Contains(out.String(), "gone"), "a gone pinned image was reported:\n%s", out.String())

	// a user an earlier step of the same clean takes, or would, frees it.
	dry.Reset()
	require.NoError(t, cleanCached(context.Background(), c, c, CleanOptions{Place: "docker", All: true, DryRun: true, Report: TextReport(&dry)},
		map[string]bool{"sandbox agent-1": true}))
	assert.ContainsString(t, dry.String(), "would remove docker image "+proxyRepo+":v1 (cached)\n")

	// someone's own image on the pats build carries its label, and keeps it
	// and everything under it, forced or not.
	mine := image("sha256:mine", []string{"mine:1"}, "h2", "l1", "l2", "l3", "l4")
	r, calls = fakeImageDaemon(t, []daemonImage{base, parent, child, mine}, nil)
	c = newContainer(testProcess(t), r, Options{Driver: "docker"})
	out.Reset()
	_, err = testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", Force: true, Report: TextReport(&out)})
	require.NoError(t, err)
	assert.Len(t, *calls, 0)
	assert.Equal(t, out.String(), "docker: kept image pats-sbx:a (built on by mine:1)\n"+
		"docker: kept image parent (built on by mine:1)\n"+
		"docker: kept image "+sandboxRepo+":v1 (built on by mine:1)\n")

	// one that only sets metadata has the same layers: neither is taken by
	// its label, and the pats build a pin names stays under it.
	meta := image("sha256:meta", []string{"meta:1"}, "h2", "l1", "l2", "l3")
	r, calls = fakeImageDaemon(t, []daemonImage{base, parent, child, meta}, nil)
	c = newContainer(testProcess(t), r, Options{Driver: "docker"})
	out.Reset()
	_, err = testProcess(t).clean(context.Background(), c, CleanOptions{Place: "docker", Force: true, Report: TextReport(&out),
		Pinned: []Resource{{Kind: "image", Name: "sha256:child", Label: "pats-sbx:a"}}})
	require.NoError(t, err)
	assert.Len(t, *calls, 0)
	assert.Equal(t, out.String(), "docker: kept image parent (built on by meta:1)\n"+
		"docker: kept image "+sandboxRepo+":v1 (built on by meta:1)\n"+
		"docker: kept image pats-sbx:a (built on by meta:1)\n")
}
