package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func (h *removingHost) RemoveImage(_ context.Context, id string) error {
	if h.inUse[id] {
		return errors.New("image is being used by a stopped container")
	}
	h.removed = append(h.removed, id)
	return nil
}

// lxdRemovingHost adds an lxd server's image use.
type lxdRemovingHost struct {
	*removingHost
	use map[string]sandbox.ImageUse
}

func (h *lxdRemovingHost) Usage(_ context.Context, id string) (sandbox.ImageUse, error) {
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

// container images go when no listed project pins them; one another project
// pins, one a container uses, and one on a daemon not answering now stay
// on the list, and a later init tries them again.
func TestRemoveSupersededContainers(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	me, other := t.TempDir(), t.TempDir()
	require.NoError(t, updatePins(other, func(f *pinFile) error {
		f.setPin("docker:default", "xxx", &pinEntry{Image: pinImage{ID: "shared"}}, now)
		return nil
	}))
	h := &removingHost{fakeHost: newFakeHost(), inUse: map[string]bool{"busy": true}}
	useTargets(t, map[string]imageHost{"docker:default": h})
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{
		"docker:default": {"sbx": {Image: pinImage{ID: "current"}}},
	}, Superseded: []superseded{
		{Target: "docker:default", ID: "old"},
		{Target: "docker:default", ID: "shared"},
		{Target: "docker:default", ID: "busy"},
		{Target: "docker:other", ID: "elsewhere"},
		{Target: "docker:default", ID: "current"}, // pinned again since
	}}
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false, 0})
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
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false, 0})
	assert.EqualArrays(t, h.removed, []string{"old", "shared", "busy"})

	// a project whose pins cannot be read keeps every container image.
	f.Superseded = []superseded{{Target: "docker:default", ID: "later"}}
	require.NoError(t, os.WriteFile(filepath.Join(other, ".pats", pinsName), []byte("{"), 0o644))
	removeSuperseded(context.Background(), f, me, []string{me, other}, now, logw{&out, false, 0})
	assert.Len(t, f.Superseded, 1)
}

// an lxd image goes only once 30 days pass without a use and no run holds
// it under its private alias; other machines' pins are unreachable.
func TestRemoveSupersededLXD(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	h := &lxdRemovingHost{removingHost: &removingHost{fakeHost: newFakeHost()}, use: map[string]sandbox.ImageUse{
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
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false, 0})
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
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "y", Pulled: true}}, now)
	assert.Len(t, f.Superseded, 1)
	f.setPin("t", "bxx", &pinEntry{Image: pinImage{ID: "x"}}, now)
	assert.Len(t, f.Superseded, 0)
	assert.That(t, f.entry("t", "bxx").Image.Pulled, "the re-pinned image lost its ownership")
	f.dropMissing(map[string]bool{"axx": true}, now)
	assert.Len(t, f.Superseded, 1) // and goes back on the list when b drops it
}

// vanishingHost loses an image once init has pinned it, before the pin is
// written: another project's init removed it in between.
type vanishingHost struct {
	*fakeHost
}

func (h vanishingHost) Pull(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error) {
	img, fetched, err := h.fakeHost.Pull(ctx, ref, force, log)
	delete(h.refs, ref) // another init removes it once this one has it
	return img, fetched, err
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
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false, 0})
	assert.Len(t, f.Superseded, 1)
	assert.ContainsString(t, out.String(), "kept superseded image x (lxd:srv): dial: connection refused")

	h.gone = true
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false, 0})
	assert.Len(t, f.Superseded, 0)
}

type lxdErrHost struct {
	*fakeHost
	gone bool
}

func (h *lxdErrHost) RemoveImage(context.Context, string) error { return nil }
func (h *lxdErrHost) Usage(context.Context, string) (sandbox.ImageUse, error) {
	if h.gone {
		return sandbox.ImageUse{}, fmt.Errorf("%w: Image not found", sandbox.ErrNoImage)
	}
	return sandbox.ImageUse{}, errors.New("dial: connection refused")
}

// two sandboxes on one image, pulled for the first: when the first moves
// off it, the second's pin becomes pats's own, so dropping the second later
// lists the image again.
func TestRemoveHandsOwnershipToTheOtherPin(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("t", "bxx", &pinEntry{Image: pinImage{ID: "x"}}, now)
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "y", Pulled: true}}, now)
	useTargets(t, map[string]imageHost{"t": &removingHost{fakeHost: newFakeHost()}})
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false, 0})
	assert.Len(t, f.Superseded, 0)
	assert.That(t, f.entry("t", "bxx").Image.Pulled, "b's pin did not take the ownership")
	f.dropMissing(map[string]bool{"axx": true}, now)
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
	f.setPin("docker:a", "s1x", &pinEntry{Daemon: "d", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:b", "s2x", &pinEntry{Daemon: "d", Image: pinImage{ID: "x"}}, now)
	f.setPin("docker:a", "s1x", &pinEntry{Daemon: "d", Image: pinImage{ID: "y", Pulled: true}}, now)
	var out bytes.Buffer
	removeSuperseded(context.Background(), f, t.TempDir(), nil, now, logw{&out, false, 0})
	assert.Len(t, h.removed, 0)
	assert.ContainsString(t, out.String(), "this project still pins it under another target")

	// the same target, another daemon: no ownership moves.
	g := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{},
		Superseded: []superseded{{Target: "docker:a", Daemon: "old", ID: "x", Pulled: true}}}
	g.setPin("docker:a", "sxx", &pinEntry{Daemon: "new", Image: pinImage{ID: "x"}}, now)
	assert.Len(t, g.Superseded, 1)
	assert.That(t, !g.entry("docker:a", "sxx").Image.Pulled, "another daemon's image took the ownership")
}

// a pin re-made against another daemon inherits nothing from the old one,
// whose images go on the list under the old daemon.
func TestRepinOnAnotherDaemon(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("docker:a", "sxx", &pinEntry{Daemon: "A", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:a", "sxx", &pinEntry{Daemon: "B", Image: pinImage{ID: "x"}}, now)
	assert.That(t, !f.entry("docker:a", "sxx").Image.Pulled, "B's image inherited A's ownership")
	require.Equal(t, len(f.Superseded), 1)
	assert.Equal(t, f.Superseded[0].Daemon, "A")
}

// the same image superseded on two daemons is two records.
func TestSupersedePerDaemon(t *testing.T) {
	now := time.Now()
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{},
		Superseded: []superseded{{Target: "docker:a", Daemon: "A", ID: "x", Pulled: true}}}
	f.setPin("docker:a", "sxx", &pinEntry{Daemon: "B", Image: pinImage{ID: "x", Pulled: true}}, now)
	f.setPin("docker:a", "sxx", &pinEntry{Daemon: "B", Image: pinImage{ID: "y", Pulled: true}}, now)
	require.Equal(t, len(f.Superseded), 2)
	assert.Equal(t, f.Superseded[1].Daemon, "B")
}

// a published pats image already on the daemon (a local `make images`
// build) is not pats's to remove; one init pulled is.
func TestPublishedImagePresentIsNotOwned(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	pub, _ := sandbox.SandboxImage("")
	h := newFakeHost(pub)
	useHost(t, h)
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"pub"}, Agents: []string{"a-pub"}}))
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.That(t, !f.entry("fake:host", "pub").Image.owned(), "a present published image was taken as pats's")
}

// init removes nothing until the new pins are written: a pin file that
// cannot be written leaves the old pins and their images.
func TestInitRemovesOnlyAfterPinsAreWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	h := &removingHost{fakeHost: newFakeHost()}
	useHost(t, h)
	useTargets(t, map[string]imageHost{"fake:host": h})
	var out bytes.Buffer
	opts := InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}
	require.NoError(t, Init(cfg, opts))
	cfg.Sandboxes[1].Image = "my/image:2"
	pats := filepath.Join(dir, ".pats")
	require.NoError(t, os.Chmod(pats, 0o555))
	t.Cleanup(func() { os.Chmod(pats, 0o755) })
	assert.That(t, Init(cfg, opts) != nil, "init succeeded with an unwritable pin file")
	assert.Len(t, h.removed, 0)
}

// a forced pull of an image already there, unchanged, does not make it
// pats's.
func TestForcedPullOfTheSameImage(t *testing.T) {
	h := newFakeHost("u/img:1")
	h.samePull = true
	var out bytes.Buffer
	im, err := ensurePulled(context.Background(), h, "u/img:1", true, logw{&out, false, 0}, &out, h.Pull)
	require.NoError(t, err)
	assert.EqualArrays(t, h.pulls, []string{"u/img:1"})
	assert.That(t, !im.owned(), "an unchanged forced pull took the user's image")
}

// a pull that lands on an image the daemon already held untagged does not
// make it pats's.
func TestPullOntoAnUntaggedImage(t *testing.T) {
	h := newFakeHost()
	h.untagged = map[string]bool{"sha256:x": true}
	h.pullTo = "sha256:x"
	var out bytes.Buffer
	im, err := ensurePulled(context.Background(), h, "u/img:1", false, logw{&out, false, 0}, &out, h.Pull)
	require.NoError(t, err)
	assert.Equal(t, im.ID, "sha256:x")
	assert.That(t, !im.owned(), "a pull onto a held image took it as pats's")
}
