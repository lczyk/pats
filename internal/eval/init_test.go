package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/images"
	"github.com/lczyk/pats/src/sandbox"
)

const initCfg = `
sb:
  kind: sandbox
  items:
    pub: {}
    own: {image: my/image:1}
    proxied: {image: my/image:1, egress: {allow: [example.com]}}
ag:
  kind: agent
  defaults: {harness: opencode-openrouter, model: m}
  items:
    a-pub: {sandbox: pub}
    a-own: {sandbox: own}
    a-proxied: {sandbox: proxied}
t: {kind: task, items: {t: {prompt: p}}}
su: {kind: suite, items: {all: {agents: ag, tasks: t}, only-pub: {agents: a-pub, tasks: t}}}
`

// init pins each sandbox's image by id: pulls what is absent, leaves what is
// there, and says pats owns an image it pulled or a published one, never a
// user's image that was already there.
func TestInitPinsContainers(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	h := newFakeHost("my/image:1")
	useHost(t, h)
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}))

	pub, _ := sandbox.SandboxImage("")
	proxy, _ := sandbox.ProxyImage("")
	assert.EqualArraysUnordered(t, h.pulls, []string{pub, proxy})

	f, err := readPins(dir)
	require.NoError(t, err)
	e := f.entry("fake:host", "pub")
	require.NotNil(t, e)
	assert.Equal(t, e.Image, pinImage{ID: "sha256:pulled-" + pub, Name: pub, Pulled: true})
	assert.Equal(t, e.Daemon, "d")
	assert.Equal(t, e.PatsBuild, patsBuild())
	own := f.entry("fake:host", "own")
	assert.Equal(t, own.Image, pinImage{ID: "my/image:1", Name: "my/image:1"})
	assert.That(t, own.Proxy == nil, "a sandbox with no egress got a proxy pin")
	prox := f.entry("fake:host", "proxied")
	require.NotNil(t, prox.Proxy)
	assert.Equal(t, *prox.Proxy, pinImage{ID: "sha256:pulled-" + proxy, Name: proxy, Pulled: true})

	// each sandbox logs its pulls and builds under .pats/init.
	log, err := os.ReadFile(filepath.Join(dir, ".pats", "init", "pub.log"))
	require.NoError(t, err)
	assert.ContainsString(t, string(log), "pulled "+pub)

	// a second init pulls nothing; --force pulls again.
	h.pulls = nil
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}))
	assert.Len(t, h.pulls, 0)
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Force: true, Sandboxes: []string{"own"}}))
	assert.EqualArrays(t, h.pulls, []string{"my/image:1"})
	f, err = readPins(dir)
	require.NoError(t, err)
	assert.That(t, f.entry("fake:host", "own").Image.Pulled, "a forced pull is pats's own")

	// and stays pats's when a later init finds it present and pulls nothing.
	h.pulls = nil
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}))
	assert.Len(t, h.pulls, 0)
	f, err = readPins(dir)
	require.NoError(t, err)
	assert.That(t, f.entry("fake:host", "own").Image.Pulled, "a re-init forgot pats pulled the image")
}

// podman's target follows the overrides its cli honours.
func TestPodmanTargetEnv(t *testing.T) {
	t.Setenv("CONTAINER_HOST", "ssh://core@box/run/podman.sock")
	target, daemon, err := containerHost{driver: "podman"}.target(context.Background())
	require.NoError(t, err)
	assert.Equal(t, target, "podman:ssh://core@box/run/podman.sock")
	assert.Equal(t, daemon, "ssh://core@box/run/podman.sock")
}

// --sandbox, --driver and run's selectors narrow what init makes; a sandbox
// no selected pair uses is an error.
func TestInitSelection(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	h := newFakeHost("my/image:1")
	useHost(t, h)
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Suites: []string{"only-pub"}}))
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.Len(t, f.Targets["fake:host"], 1)

	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Suites: []string{"only-pub"}, Sandboxes: []string{"own"}})
	assert.Error(t, err, `--sandbox "own": no selected pair uses it`)
	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Driver: "lxd"})
	assert.Error(t, err, "no sandbox to init")
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Agents: []string{"a-own"}}))
	f, err = readPins(dir)
	require.NoError(t, err)
	assert.Len(t, f.Targets["fake:host"], 2)
}

// a run stops before anything starts when an image is not pinned for where
// it would start it, pinned against another daemon, or gone; it names every
// missing sandbox and the init that makes them.
func TestRunMissingPins(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, initCfg)
	h := newFakeHost("my/image:1")
	useHost(t, h)
	var out bytes.Buffer
	run := func() error {
		_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
		return err
	}

	assert.Error(t, run(), "run `pats init` first") // nothing pinned: bare init
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}))

	// the proxied sandbox's proxy is gone, the own sandbox's image too.
	pinned, _ := readPins(dir)
	proxyID := pinned.entry("fake:host", "proxied").Proxy.ID
	for ref, id := range h.refs {
		if id == proxyID || ref == "my/image:1" {
			delete(h.refs, ref)
		}
	}
	err := run()
	assert.Error(t, err, "images missing for 2 sandbox(es)")
	assert.ContainsString(t, err.Error(), "own: its pinned image my/image:1 is gone")
	assert.ContainsString(t, err.Error(), "proxied: its pinned image my/image:1 is gone")
	assert.ContainsString(t, err.Error(), "run `pats init --sandbox own --sandbox proxied` first")

	// another daemon answering: every pin is missing.
	h.daemon = "other"
	err = run()
	assert.ContainsString(t, err.Error(), "pub: pinned against another docker daemon")
	assert.ContainsString(t, err.Error(), "run `pats init` first")
	h.daemon = "d"
	h.targetName = "fake:elsewhere"
	assert.ContainsString(t, run().Error(), "not pinned for fake:elsewhere")
}

// a pin that moves puts the images pats made and the new pin dropped on the
// superseded list, once; a sandbox gone from the config takes its pin with
// it; images pats did not make never go on the list.
func TestPinsSupersede(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{}}
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "i1", Built: true}, Layers: []string{"l1"},
		Proxy: &pinImage{ID: "p1", Pulled: true}}, now)
	f.setPin("t", "b", &pinEntry{Image: pinImage{ID: "user"}}, now)
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "i2", Built: true}, Layers: []string{"l1"},
		Proxy: &pinImage{ID: "p1", Pulled: true}}, now)
	f.setPin("t", "a", &pinEntry{Image: pinImage{ID: "i2", Built: true}, Layers: []string{"l1"}}, now)
	var ids []string
	for _, s := range f.Superseded {
		ids = append(ids, s.ID)
	}
	assert.EqualArrays(t, ids, []string{"i1", "p1"})

	f.dropMissing(map[string]bool{"b": true}, now)
	assert.That(t, f.entry("t", "a") == nil, "a dropped sandbox kept its pin")
	ids = nil
	for _, s := range f.Superseded {
		ids = append(ids, s.ID)
	}
	assert.EqualArraysUnordered(t, ids, []string{"i1", "p1", "i2", "l1"})
	f.dropMissing(map[string]bool{}, now) // the user's image never goes on the list
	assert.Len(t, f.Superseded, 4)
}

// the file round-trips, and concurrent writers each keep their own entries.
func TestPinsFile(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			require.NoError(t, updatePins(dir, func(f *pinFile) error {
				f.setPin("docker:default", "sb"+string(rune('a'+i)), &pinEntry{Image: pinImage{ID: "x"}}, time.Now())
				return nil
			}))
		}(i)
	}
	wg.Wait()
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.Len(t, f.Targets["docker:default"], 8)

	b, err := os.ReadFile(filepath.Join(dir, ".pats", pinsName))
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Equal(t, string(raw["version"]), "1")
	_, ok := raw["docker:default"]
	assert.That(t, ok, "targets are top-level keys: %s", b)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pats", pinsName), []byte(`{"version": 9}`), 0o644))
	_, err = readPins(dir)
	assert.Error(t, err, "run `pats init`")
}

// init stamps each image's harness versions into its pin; a run warns when
// they are not what this pats pins, and records them in each pair's
// metadata.
func TestComponentStamps(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, initCfg)
	h := newFakeHost("my/image:1")
	pins := images.ComponentPins()
	h.stamped = map[string]string{"claude": pins["claude"]}
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}))
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.EqualMaps(t, f.entry("fake:host", "own").Components, map[string]string{"claude": pins["claude"]})

	out.Reset()
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}})
	require.NoError(t, err)
	assert.That(t, !bytes.Contains(out.Bytes(), []byte("harnesses differ")), "pinned versions warned: %s", out.String())
	meta, err := os.ReadFile(filepath.Join(runDir, "a-own", "t", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"claude": "`+pins["claude"]+`"`)

	// a stamp that fails fails that sandbox's init and keeps its old pin.
	h.stampErr = errNoImage
	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}})
	assert.Error(t, err, "init failed for sandbox(es) own")
	f, err = readPins(dir)
	require.NoError(t, err)
	assert.EqualMaps(t, f.entry("fake:host", "own").Components, map[string]string{"claude": pins["claude"]})
	h.stampErr = nil

	h.stamped = map[string]string{"claude": "0.0.1"}
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}))
	out.Reset()
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), `sandbox "own": its harnesses differ from what this pats pins: claude 0.0.1, pinned `+pins["claude"])
}
