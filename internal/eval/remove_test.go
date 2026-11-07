package eval

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// removingHost is a fake host that removes images, refusing those in use.
type removingHost struct {
	*fakeHost
	inUse   map[string]bool
	removed []string
}

func (h *removingHost) remove(_ context.Context, id string) error {
	if h.inUse[id] {
		return errors.New("image is being used by a stopped container")
	}
	h.removed = append(h.removed, id)
	return nil
}

// lxdRemovingHost adds an lxd server's image use.
type lxdRemovingHost struct {
	*removingHost
	use map[string]sandbox.LXDImageUse
}

func (h *lxdRemovingHost) usage(_ context.Context, id string) (sandbox.LXDImageUse, error) {
	return h.use[id], nil
}

func useTargets(t *testing.T, hosts map[string]imageHost) {
	t.Helper()
	old := hostForTarget
	hostForTarget = func(_ context.Context, target, _ string) (imageHost, bool) {
		h, ok := hosts[target]
		return h, ok
	}
	t.Cleanup(func() { hostForTarget = old })
}

// container images go when no listed project pins them; one another project
// pins, one a container uses, and one on a daemon not answering now stay
// on the list, and a later init tries them again.
func TestRemoveSupersededContainers(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	me, other := t.TempDir(), t.TempDir()
	require.NoError(t, updatePins(other, func(f *pinFile) error {
		f.setPin("docker:default", "x", &pinEntry{Image: pinImage{ID: "shared"}}, now)
		return nil
	}))
	h := &removingHost{fakeHost: newFakeHost(), inUse: map[string]bool{"busy": true}}
	useTargets(t, map[string]imageHost{"docker:default": h})
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{
		"docker:default": {"sb": {Image: pinImage{ID: "current"}}},
	}, Superseded: []superseded{
		{Target: "docker:default", ID: "old"},
		{Target: "docker:default", ID: "shared"},
		{Target: "docker:default", ID: "busy"},
		{Target: "docker:other", ID: "elsewhere"},
		{Target: "docker:default", ID: "current"}, // pinned again since
	}}
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false})
	assert.EqualArrays(t, h.removed, []string{"old"})
	var left []string
	for _, s := range f.Superseded {
		left = append(left, s.ID)
	}
	assert.EqualArrays(t, left, []string{"shared", "busy", "elsewhere"})
	assert.ContainsString(t, out.String(), "kept superseded image shared (docker:default): another project on this machine pins it")
	assert.ContainsString(t, out.String(), "kept superseded image busy (docker:default): image is being used")
	assert.ContainsString(t, out.String(), "kept superseded image elsewhere (docker:other): its docker:other is not the one answering now")

	// the container went, the other project dropped it: the next init takes both.
	h.inUse = nil
	require.NoError(t, updatePins(other, func(f *pinFile) error {
		f.Targets = map[string]map[string]*pinEntry{}
		return nil
	}))
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false})
	assert.EqualArrays(t, h.removed, []string{"old", "shared", "busy"})

	// a project whose pins cannot be read keeps every container image.
	f.Superseded = []superseded{{Target: "docker:default", ID: "later"}}
	require.NoError(t, os.WriteFile(filepath.Join(other, ".pats", pinsName), []byte("{"), 0o644))
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false})
	assert.Len(t, f.Superseded, 1)
}

// an lxd image goes only once 30 days pass without a use and no run holds
// it under its private alias; other machines' pins are unreachable.
func TestRemoveSupersededLXD(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	h := &lxdRemovingHost{removingHost: &removingHost{fakeHost: newFakeHost()}, use: map[string]sandbox.LXDImageUse{
		"stale":   {LastUsed: now.Add(-31 * 24 * time.Hour)},
		"recent":  {LastUsed: now.Add(-3 * 24 * time.Hour)},
		"unused":  {Uploaded: now.Add(-40 * 24 * time.Hour)},
		"new":     {Uploaded: now.Add(-time.Hour)},
		"private": {Uploaded: now.Add(-40 * 24 * time.Hour), RunPrivate: true},
	}}
	useTargets(t, map[string]imageHost{"lxd:srv": h})
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	for _, id := range []string{"stale", "recent", "unused", "new", "private"} {
		f.Superseded = append(f.Superseded, superseded{Target: "lxd:srv", ID: id})
	}
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false})
	assert.EqualArrays(t, h.removed, []string{"stale", "unused"})
	assert.ContainsString(t, out.String(), "kept superseded image recent (lxd:srv): used 72h0m0s ago, may serve another machine")
	assert.ContainsString(t, out.String(), "kept superseded image private (lxd:srv): a run holds it under its private alias")
	assert.Len(t, f.Superseded, 3)
}

// the projects file lists each project once and forgets ones that are gone.
func TestRegisterProject(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	_, err := registerProject(a)
	require.NoError(t, err)
	got, err := registerProject(b)
	require.NoError(t, err)
	assert.EqualArrays(t, got, []string{a, b})
	got, err = registerProject(a)
	require.NoError(t, err)
	assert.EqualArrays(t, got, []string{a, b})
	require.NoError(t, os.RemoveAll(a))
	got, err = registerProject(b)
	require.NoError(t, err)
	assert.EqualArrays(t, got, []string{b})
}

// an init that moves a pin removes the image pats pulled for the old one.
func TestInitRemovesWhatItReplaces(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	h := &removingHost{fakeHost: newFakeHost()}
	useHost(t, h)
	useTargets(t, map[string]imageHost{"fake:host": h})
	var out bytes.Buffer
	opts := InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}
	require.NoError(t, Init(cfg, opts))
	first, _ := readPins(dir)
	old := first.entry("fake:host", "own").Image.ID

	cfg.Sandboxes[1].Image = "my/image:2"
	require.NoError(t, Init(cfg, opts))
	assert.EqualArrays(t, h.removed, []string{old})
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.Len(t, f.Superseded, 0)
	assert.ContainsString(t, out.String(), "removed superseded image")
}

// an image pats pulled for one sandbox and another sandbox pins as found
// stays pats's: the second pin takes it off the list, owned.
func TestPinsReclaimOwnership(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "y", Pulled: true}}, now)
	assert.Len(t, f.Superseded, 1)
	f.setPin("t", "b", &pinEntry{Image: pinImage{ID: "x"}}, now)
	assert.Len(t, f.Superseded, 0)
	assert.That(t, f.entry("t", "b").Image.Pulled, "the re-pinned image lost its ownership")
	f.dropMissing(map[string]bool{"a": true}, now)
	assert.Len(t, f.Superseded, 1) // and goes back on the list when b drops it
}

// vanishingHost loses an image right after init inspects it: another
// project's init removed it before this one's pin was written.
type vanishingHost struct{ *fakeHost }

func (h vanishingHost) inspect(ctx context.Context, ref string) (string, string, error) {
	id, digest, err := h.fakeHost.inspect(ctx, ref)
	delete(h.refs, ref)
	return id, digest, err
}

// an image gone between being made and being pinned is a failed init, not
// a pin to an image that is not there.
func TestInitImageRemovedBeforePin(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	useHost(t, vanishingHost{newFakeHost("my/image:1")})
	var out bytes.Buffer
	err := Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}})
	assert.Error(t, err, "init failed for sandbox(es) own")
	assert.ContainsString(t, out.String(), "was removed since this init made it")
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.That(t, f.entry("fake:host", "own") == nil, "a vanished image was pinned")
}

// a lookup that fails is not an image that is gone: it stays listed.
func TestRemoveKeepsOnLookupFailure(t *testing.T) {
	now := time.Now()
	h := &lxdErrHost{fakeHost: newFakeHost()}
	useTargets(t, map[string]imageHost{"lxd:srv": h})
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{},
		Superseded: []superseded{{Target: "lxd:srv", ID: "x"}}}
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false})
	assert.Len(t, f.Superseded, 1)
	assert.ContainsString(t, out.String(), "kept superseded image x (lxd:srv): dial: connection refused")

	h.gone = true
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false})
	assert.Len(t, f.Superseded, 0)
}

type lxdErrHost struct {
	*fakeHost
	gone bool
}

func (h *lxdErrHost) remove(context.Context, string) error { return nil }
func (h *lxdErrHost) usage(context.Context, string) (sandbox.LXDImageUse, error) {
	if h.gone {
		return sandbox.LXDImageUse{}, errors.New("Image not found")
	}
	return sandbox.LXDImageUse{}, errors.New("dial: connection refused")
}

func TestGoneMsg(t *testing.T) {
	assert.That(t, goneMsg(errors.New("Error response from daemon: No such image: x")), "docker's gone")
	assert.That(t, goneMsg(errors.New("x: image not known")), "podman's gone")
	assert.That(t, goneMsg(errors.New("lxc query srv:/1.0/images/x: exit status 1 (Error: Image not found)")), "lxd's gone")
	assert.That(t, !goneMsg(errors.New("Cannot connect to the Docker daemon")), "an unreachable daemon read as gone")
	assert.That(t, !goneMsg(errors.New(`exec: "lxc": executable file not found in $PATH`)), "a missing cli read as gone")
	assert.That(t, !goneMsg(errors.New("Error: Project not found")), "a missing project read as gone")
}

// two sandboxes on one image, pulled for the first: when the first moves
// off it, the second's pin becomes pats's own, so dropping the second later
// lists the image again.
func TestRemoveHandsOwnershipToTheOtherPin(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("t", "b", &pinEntry{Image: pinImage{ID: "x"}}, now)
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "y", Pulled: true}}, now)
	useTargets(t, map[string]imageHost{"t": &removingHost{fakeHost: newFakeHost()}})
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false})
	assert.Len(t, f.Superseded, 0)
	assert.That(t, f.entry("t", "b").Image.Pulled, "b's pin did not take the ownership")
	f.dropMissing(map[string]bool{"a": true}, now)
	assert.Len(t, f.Superseded, 1)
}

// a listed project this process cannot look at stays listed; only one that
// is gone is forgotten.
func TestRegisterProjectKeepsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	parent := t.TempDir()
	locked := filepath.Join(parent, "p")
	require.NoError(t, os.Mkdir(locked, 0o755))
	_, err := registerProject(locked)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o000))
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	got, err := registerProject(t.TempDir())
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(got, "\n"), locked)
}

// this project's pin of the same image under another target keeps it, and
// ownership moves only between pins of one daemon.
func TestRemoveAcrossTargetsAndDaemons(t *testing.T) {
	now := time.Now()
	h := &removingHost{fakeHost: newFakeHost()}
	useTargets(t, map[string]imageHost{"docker:a": h, "docker:b": h})
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("docker:a", "s1", &pinEntry{Daemon: "d", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:b", "s2", &pinEntry{Daemon: "d", Image: pinImage{ID: "x"}}, now)
	f.setPin("docker:a", "s1", &pinEntry{Daemon: "d", Image: pinImage{ID: "y", Pulled: true}}, now)
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false})
	assert.Len(t, h.removed, 0)
	assert.ContainsString(t, out.String(), "this project still pins it under another target")

	// the same target, another daemon: no ownership moves.
	g := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{},
		Superseded: []superseded{{Target: "docker:a", Daemon: "old", ID: "x", Pulled: true}}}
	g.setPin("docker:a", "s", &pinEntry{Daemon: "new", Image: pinImage{ID: "x"}}, now)
	assert.Len(t, g.Superseded, 1)
	assert.That(t, !g.entry("docker:a", "s").Image.Pulled, "another daemon's image took the ownership")
}

// a pin re-made against another daemon inherits nothing from the old one,
// whose images go on the list under the old daemon.
func TestRepinOnAnotherDaemon(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("docker:a", "s", &pinEntry{Daemon: "A", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:a", "s", &pinEntry{Daemon: "B", Image: pinImage{ID: "x"}}, now)
	assert.That(t, !f.entry("docker:a", "s").Image.Pulled, "B's image inherited A's ownership")
	require.Equal(t, len(f.Superseded), 1)
	assert.Equal(t, f.Superseded[0].Daemon, "A")
}

// the same image superseded on two daemons is two records.
func TestSupersedePerDaemon(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{},
		Superseded: []superseded{{Target: "docker:a", Daemon: "A", ID: "x", Pulled: true}}}
	f.setPin("docker:a", "s", &pinEntry{Daemon: "B", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:a", "s", &pinEntry{Daemon: "B", Image: pinImage{ID: "y", Pulled: true}}, now)
	require.Equal(t, len(f.Superseded), 2)
	assert.Equal(t, f.Superseded[1].Daemon, "B")
}
