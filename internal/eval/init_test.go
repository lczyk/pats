package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/lczyk/pats/internal/machine"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

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
	cfg.Harnesses.Harness("opencode-openrouter").CLI = "" // nothing to place: the default image is pulled as it is
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
	pins := map[string]string{}
	for name, c := range machine.Defaults().CLIs {
		pins[name] = c.Version
	}
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
		assert.Equal(t, initWhat(c.sb, dir, false), c.want, c.sb.Driver)
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

// a sandbox pats composes gets the clis its agents run placed in its image:
// the base is pulled and asked its arch, each cli's artefact for it is
// fetched, and the image is built on the base with them. the pin records
// what was placed, a run records it in each pair, and a pin that moves since
// is a warning naming the init.
func TestInitPlacesHarnesses(t *testing.T) {
	dir := t.TempDir()
	const body = `
sbx: {kind: sandbox, items: {dflt: {}, user: {image: my/image:1}}}
agx:
  kind: agent
  defaults: {model: m}
  items:
    oc: {harness: opencode-openrouter, sandbox: dflt}
    cc: {harness: claude-cli-keyless, sandbox: dflt}
    uc: {harness: codex-cli-keyless, sandbox: user}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`
	cfg := parseCfg(t, body)
	h := newFakeHost("my/image:1")
	h.arch = "arm64"
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var fetched []string
	old := fetchArtefact
	fetchArtefact = func(_ context.Context, a sandbox.Artefact, _ *http.Client) (string, error) {
		fetched = append(fetched, a.CLI+" "+a.Version+" "+a.Arch)
		return "/cache/" + a.SHA256, nil
	}
	defer func() { fetchArtefact = old }()

	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Agents: []string{"oc"}}), "%s", out.String())
	assert.ContainsString(t, out.String(), `sandbox "dflt": building images`)

	// the whole config's agents decide the set, not the selection: claude
	// is placed for an init of the opencode agent alone. an image: gets none.
	claude, opencode := cfg.Harnesses.CLIs["claude"], cfg.Harnesses.CLIs["opencode"]
	assert.EqualArrays(t, fetched, []string{"claude " + claude.Version + " arm64", "opencode " + opencode.Version + " arm64"})
	pub, _ := sandbox.SandboxImage("")
	assert.EqualArrays(t, h.pulls, []string{pub})
	require.Len(t, h.builds, 1)
	spec := h.builds[0]
	assert.Equal(t, spec.Base, "") // on the pats sandbox
	require.Len(t, spec.Harness, 2)
	_, art, _ := claude.ArtefactFor("arm64", "")
	assert.Equal(t, spec.Harness[0].Artefact.SHA256, art.SHA256())
	assert.Equal(t, spec.Harness[0].File, "/cache/"+art.SHA256())
	assert.Equal(t, spec.Harness[0].Artefact.Bin, "package/claude")

	f, err := readPins(dir)
	require.NoError(t, err)
	e := f.entry("fake:host", "dflt")
	require.NotNil(t, e)
	assert.Equal(t, e.Arch, "arm64")
	assert.Equal(t, e.Image.Built, true)
	assert.EqualMaps(t, e.Harnesses["claude"], map[string]string{"version": claude.Version, "hash": art.Hash, "tree": spec.Harness[0].Artefact.TreeID()})
	assert.That(t, e.Inputs["harnesses"] != "", "the pin has no input for its harnesses")

	// an image: is the user's whole statement of what is inside: nothing is
	// fetched for it or placed in it.
	fetched = nil
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"user"}}), "%s", out.String())
	assert.Len(t, fetched, 0)
	require.Len(t, h.builds, 1)
	f, err = readPins(dir)
	require.NoError(t, err)
	user := f.entry("fake:host", "user")
	require.NotNil(t, user)
	assert.Equal(t, user.Arch, "")
	assert.Len(t, user.Harnesses, 0)

	// a run records what was placed, and says nothing of a fresh pin. (a
	// run writes the pinned image into its config: each gets its own.)
	out.Reset()
	runDir, err := Run(parseCfg(t, body), Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"oc"}})
	require.NoError(t, err, "%s", out.String())
	assert.That(t, !strings.Contains(out.String(), "changed since its pats init"), "%s", out.String())
	m := readMeta(t, runDir, "oc", "txx")
	assert.EqualMaps(t, m.Placed["opencode"], e.Harnesses["opencode"])
	assert.Len(t, m.Placed, 2) // every cli of the image, not only the agent's

	// a pin that moved since init: the run warns and goes on with the image.
	moved := parseCfg(t, body)
	moved.Harnesses.CLIs["claude"].Version = "9.9.9"
	out.Reset()
	_, err = Run(moved, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"oc"}})
	require.NoError(t, err, "%s", out.String())
	assert.ContainsString(t, out.String(), `sandbox "dflt": changed since its pats init: its harness clis or a pin of one; running the pinned image (pats init --sandbox dflt to rebuild)`)

	// a forced init pulls the pats sandbox afresh before it builds on it, as
	// it does when that image is the sandbox's own.
	out.Reset()
	require.NoError(t, Init(parseCfg(t, body), InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"dflt"}, Force: true}), "%s", out.String())
	assert.EqualArrays(t, h.pulls, []string{pub, pub})
	assert.Equal(t, h.builds[len(h.builds)-1].Force, true)

	// a config built by hand has no tables of its own: the builtin ones
	// decide its harnesses, and its pins too.
	bare := parseCfg(t, body)
	bare.Harnesses = nil
	fetched = nil
	out.Reset()
	require.NoError(t, Init(bare, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"dflt"}}), "%s", out.String())
	assert.Len(t, fetched, 2)

	// an arch a cli has no artefact for fails that sandbox's init, naming it.
	h.arch = "amd64"
	half := parseCfg(t, body)
	delete(half.Harnesses.CLIs["opencode"].Artefacts, "https://registry.npmjs.org/opencode-linux-x64/-/opencode-linux-x64-${version}.tgz")
	out.Reset()
	err = Init(half, InitOptions{ConfigDir: dir, Out: &out, Sandboxes: []string{"dflt"}})
	assert.Error(t, err, "init failed for sandbox(es) dflt")
	assert.ContainsString(t, out.String(), "cli opencode "+opencode.Version+" has no artefact for amd64")
}

// a harness's cli that no table has is one the image brings: init places
// nothing for it, asks the image for its version as for any cli of the set,
// and fails on an image that has none.
func TestInitStampsABroughtCLI(t *testing.T) {
	dir := t.TempDir()
	const body = `
sbx: {kind: sandbox, items: {dflt: {provision: "true"}}}
hxx: {kind: harness, items: {mine: {cli: tool, run: tool "$PATS_PROMPT_FILE"}}}
agx:
  kind: agent
  items:
    oc: {harness: opencode-openrouter, model: m, sandbox: dflt}
    mm: {harness: mine, sandbox: dflt}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`
	cfg := parseCfg(t, body)
	assert.Equal(t, cfg.Harnesses.Harness("mine").Cli().Brought(), true)
	h := newFakeHost()
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var fetched []string
	old := fetchArtefact
	fetchArtefact = func(_ context.Context, a sandbox.Artefact, _ *http.Client) (string, error) {
		fetched = append(fetched, a.CLI)
		return "/cache/" + a.SHA256, nil
	}
	defer func() { fetchArtefact = old }()

	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	assert.EqualArrays(t, fetched, []string{"opencode"})
	require.Len(t, h.builds, 1)
	require.Len(t, h.builds[0].Harness, 1)

	f, err := readPins(dir)
	require.NoError(t, err)
	e := f.entry("fake:host", "dflt")
	require.NotNil(t, e)
	assert.Equal(t, e.Components["tool"], "0.0.0") // what the fake image says of a cli it knows no pin for
	assert.That(t, e.Components["opencode"] != "", "the placed cli has no stamp")
	_, placed := e.Harnesses["tool"]
	assert.Equal(t, placed, false)

	// an image that has none of it on its PATH is no image for its agents.
	h.stamped = map[string]string{"opencode": e.Components["opencode"]}
	out.Reset()
	err = Init(cfg, InitOptions{ConfigDir: dir, Out: &out, Force: true})
	require.That(t, err != nil, "init passed on an image without the cli")
	assert.ContainsString(t, out.String()+err.Error(), "its image has no tool on its PATH, which its agents run")
}

// a harness item's own pin is what init fetches and places for the sandboxes
// its agents run in, and what a run compares the image's stamps with: not
// the machine's pin of the cli. an arch the pin has no hash for fails
// init, naming the harness, the version and the arch.
func TestInitPlacesAProjectsPin(t *testing.T) {
	dir := t.TempDir()
	sha := strings.Repeat("ab", 32)
	body := `
sbx: {kind: sandbox, items: {dflt: {}}}
hxx:
  kind: harness
  items:
    claude-cli-keyless:
      version: 9.9.9
      hash: {arm64: sha256:` + sha + `}
agx: {kind: agent, defaults: {model: m}, items: {cc: {harness: claude-cli-keyless, sandbox: dflt}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`
	cfg := parseCfg(t, body)
	h := newFakeHost()
	h.arch = "arm64"
	h.stamped = map[string]string{"claude": "9.9.9"}
	useHost(t, h)
	useFakeSandbox(t, &fakeSandbox{})
	var fetched []sandbox.Artefact
	old := fetchArtefact
	fetchArtefact = func(_ context.Context, a sandbox.Artefact, _ *http.Client) (string, error) {
		fetched = append(fetched, a)
		return "/cache/" + a.SHA256, nil
	}
	defer func() { fetchArtefact = old }()

	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	require.Len(t, fetched, 1)
	assert.Equal(t, fetched[0].Version, "9.9.9")
	assert.Equal(t, fetched[0].SHA256, sha)
	assert.ContainsString(t, fetched[0].URL, "9.9.9")
	_, def, _ := machine.Defaults().CLIs["claude"].ArtefactFor("arm64", "")
	assert.Equal(t, fetched[0].Bin, def.Bin) // how it is placed is the machine's
	f, err := readPins(dir)
	require.NoError(t, err)
	e := f.entry("fake:host", "dflt")
	require.NotNil(t, e)
	assert.Equal(t, e.Harnesses["claude"]["version"], "9.9.9")

	// the image holds the item's version: that is no drift from a pin.
	out.Reset()
	_, err = Run(parseCfg(t, body), Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, "%s", out.String())
	assert.That(t, !strings.Contains(out.String(), "pinned"), "the item's own version was reported as drift: %s", out.String())

	h.arch = "amd64"
	out.Reset()
	err = Init(parseCfg(t, body), InitOptions{ConfigDir: dir, Out: &out})
	assert.Error(t, err, "init failed for sandbox(es) dflt")
	assert.ContainsString(t, out.String(), "harness claude-cli-keyless pins cli claude at 9.9.9 with no hash for amd64")
}
