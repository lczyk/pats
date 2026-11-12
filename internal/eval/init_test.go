package eval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/images"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

const initCfg = `
sbx:
  kind: sandbox
  items:
    pub: {}
    own: {image: my/image:1}
    proxied: {image: my/image:1, egress: {allow: [example.com]}}
agx:
  kind: agent
  defaults: {harness: opencode-openrouter, model: m}
  items:
    a-pub: {sandbox: pub}
    a-own: {sandbox: own}
    a-proxied: {sandbox: proxied}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}, only-pub: {agents: a-pub, tasks: txx}}}
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

// init takes .pats/lock: a run in the same project reads its pins once, and
// an init beside it could remove an image the run's next pair needs.
func TestInitTakesDirLock(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	useHost(t, newFakeHost("my/image:1"))
	unlock, err := lockConfigDir(dir)
	require.NoError(t, err)
	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &bytes.Buffer{}})
	unlock()
	require.Error(t, err, ".pats/lock held")
	_, err = os.Stat(filepath.Join(dir, ".pats", "images.json"))
	assert.That(t, os.IsNotExist(err), "init pinned under a held lock")
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
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "i1", Built: true}, Layers: []string{"l1"},
		Proxy: &pinImage{ID: "p1", Pulled: true}}, now)
	f.setPin("t", "bxx", &pinEntry{Image: pinImage{ID: "user"}}, now)
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "i2", Built: true}, Layers: []string{"l1"},
		Proxy: &pinImage{ID: "p1", Pulled: true}}, now)
	f.setPin("t", "axx", &pinEntry{Image: pinImage{ID: "i2", Built: true}, Layers: []string{"l1"}}, now)
	var ids []string
	for _, s := range f.Superseded {
		ids = append(ids, s.ID)
	}
	assert.EqualArrays(t, ids, []string{"i1", "p1"})

	f.dropMissing(map[string]bool{"bxx": true}, now)
	assert.That(t, f.entry("t", "axx") == nil, "a dropped sandbox kept its pin")
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

// init stamps the versions of the clis a sandbox's agents run into its pin;
// a run warns when they are not what is pinned, and records them in each
// pair's metadata.
func TestComponentStamps(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, initCfg)
	h := newFakeHost("my/image:1")
	pins := images.ComponentPins()
	h.stamped = map[string]string{"opencode": pins["opencode"]}
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}))
	f, err := readPins(dir)
	require.NoError(t, err)
	assert.EqualMaps(t, f.entry("fake:host", "own").Components, map[string]string{"opencode": pins["opencode"]})

	out.Reset()
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}})
	require.NoError(t, err)
	assert.That(t, !bytes.Contains(out.Bytes(), []byte("harnesses differ")), "pinned versions warned: %s", out.String())
	meta, err := os.ReadFile(filepath.Join(runDir, "a-own", "txx", "metadata.json"))
	require.NoError(t, err)
	assert.ContainsString(t, string(meta), `"opencode": "`+pins["opencode"]+`"`)

	// a stamp that fails fails that sandbox's init and keeps its old pin.
	h.stampErr = errNoImage
	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}})
	assert.Error(t, err, "init failed for sandbox(es) own")
	f, err = readPins(dir)
	require.NoError(t, err)
	assert.EqualMaps(t, f.entry("fake:host", "own").Components, map[string]string{"opencode": pins["opencode"]})
	h.stampErr = nil

	h.stamped = map[string]string{"opencode": "0.0.1"}
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"own"}, Agents: []string{"a-own"}}))
	out.Reset()
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), `sandbox "own": its harnesses differ from what is pinned: opencode 0.0.1, pinned `+pins["opencode"])
}

// init's opening line for a sandbox says whether it builds (and where that
// logs) or only pulls.
func TestInitWhat(t *testing.T) {
	dir := "/nonexistent/proj"
	log := relToCwd(filepath.Join(dir, ".pats", "init", "sxx.log"))
	cases := []struct {
		sb   config.Sandbox
		want string
	}{
		{config.Sandbox{Name: "sxx", Driver: "docker"}, "pulling images"},
		{config.Sandbox{Name: "sxx", Driver: "docker", Image: "my/image:1"}, "pulling images"},
		{config.Sandbox{Name: "sxx", Driver: "docker", Provision: "provision"}, "building images (log: " + log + ")"},
		{config.Sandbox{Name: "sxx", Driver: "podman", Build: "."}, "building images (log: " + log + ")"},
		{config.Sandbox{Name: "sxx", Driver: "lxd"}, "building images (log: " + log + ")"},
		{config.Sandbox{Name: "sxx", Driver: "lxd", Image: "ubuntu:26.04"}, "pulling images"},
		{config.Sandbox{Name: "sxx", Driver: "lxd", Image: "my-alias"}, "pinning its image"},
	}
	for _, c := range cases {
		assert.Equal(t, initWhat(c.sb, dir), c.want, c.sb.Driver)
	}
}

// a judge's sandbox is used by the suites whose agent scorers name it, and
// -a narrows the task pairs without dropping it.
func TestInitSandboxesCountsJudges(t *testing.T) {
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, jsb: {image: j}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: jsb}}}
txx: {kind: task, items: {txx: {prompt: p}}}
scx: {kind: scorer, items: {jud: {agent: jxx, prompt: grade}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx, scorers: jud}}}
`)
	sbs, err := initSandboxes(cfg, InitOptions{Agents: []string{"axx"}})
	require.NoError(t, err)
	var names []string
	for _, sb := range sbs {
		names = append(names, sb.Name)
	}
	assert.EqualArrays(t, names, []string{"sxx", "jsb"})
	_, err = initSandboxes(cfg, InitOptions{Sandboxes: []string{"jsb"}})
	require.NoError(t, err)
}

// a step sandbox is pinned with the rest: a task's when its pairs are
// selected, a scorer's when its suite is.
func TestInitSandboxesCountsStepSandboxes(t *testing.T) {
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, tst: {image: t}, sst: {image: c}, jst: {image: j}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tks: {kind: task, items: {txx: {prompt: p, stage: ./s, step-sandbox: tst}, tyy: {prompt: p}}}
scx: {kind: scorer, items: {scr: {score: ./sc, step-sandbox: sst}, jud: {agent: axx, prompt: g, stage: ./s, step-sandbox: jst}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tks, scorers: scx}}}
`)
	names := func(o InitOptions) []string {
		sbs, err := initSandboxes(cfg, o)
		require.NoError(t, err)
		var out []string
		for _, sb := range sbs {
			out = append(out, sb.Name)
		}
		return out
	}
	assert.EqualArrays(t, names(InitOptions{}), []string{"sxx", "tst", "sst", "jst"})
	// -t narrows the task pairs, not the scorers.
	assert.EqualArrays(t, names(InitOptions{Tasks: []string{"tyy"}}), []string{"sxx", "sst", "jst"})
	assert.EqualArrays(t, names(InitOptions{Sandboxes: []string{"tst"}}), []string{"tst"})
}

// an image has to hold the clis its sandbox's agents run: init asks it, and
// fails naming the ones it lacks. a run whose agent's cli the pinned image
// was made without stops before anything starts, naming the init to run.
func TestInitAndRunNeedTheSandboxCLIs(t *testing.T) {
	dir := t.TempDir()
	const body = `
sbx: {kind: sandbox, items: {own: {image: my/image:1}}}
agx:
  kind: agent
  defaults: {sandbox: own, model: m}
  items:
    oc: {harness: opencode-openrouter}
    cc: {harness: claude-cli-keyless}
    floor: {harness: noop, model: ~}
hxx: {kind: harness, items: {noop: {run: "true"}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`
	cfg := parseCfg(t, body)
	// the set is every agent's cli, whatever is selected, and a harness with no cli adds none.
	assert.Equal(t, strings.Join(sandboxCLIs(cfg, "own"), " "), "claude opencode")

	h := newFakeHost("my/image:1")
	h.stamped = map[string]string{"opencode": "1.19.5"} // no claude in it
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var out bytes.Buffer
	err := Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Agents: []string{"oc"}})
	assert.Error(t, err, "init failed for sandbox(es) own")
	assert.ContainsString(t, out.String(), `sandbox "own": its image has no claude on its PATH, which its agents run`)

	// pinned when the image held only opencode: a run of the opencode
	// agent goes, one of the claude agent stops.
	h.stamped = nil
	one := parseCfg(t, strings.Replace(body, "    cc: {harness: claude-cli-keyless}\n", "", 1))
	require.NoError(t, Init(one, InitOptions{ConfigDir: dir, Out: &out}))
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"oc", "floor"}})
	require.NoError(t, err, "%s", out.String())
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"cc"}})
	assert.Error(t, err, "own: its image was made without claude")
	assert.Error(t, err, "run `pats init` first")
}
