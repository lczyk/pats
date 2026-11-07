package eval

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
