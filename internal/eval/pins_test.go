package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lczyk/pats/images"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// fakeHost is an image host in memory: what it holds by ref, each with an
// id, and the pulls it was asked for.
type fakeHost struct {
	targetName, daemon string
	refs               map[string]string // ref -> id
	pulls              []string
	stamped            map[string]string // the stamps every image carries
	stampErr           error
	samePull           bool              // a pull finds the image it already has
	untagged           map[string]bool   // ids held with no ref
	pullTo             string            // the id a pull lands on, when set
	digests            map[string]string // ref -> registry digest
	cpus               int
	upstream           func(ref string) (string, error)
}

func newFakeHost(refs ...string) *fakeHost {
	h := &fakeHost{targetName: "fake:host", daemon: "d", refs: map[string]string{}}
	for _, r := range refs {
		h.refs[r] = r
	}
	return h
}

func (h *fakeHost) Target(context.Context) (sandbox.Target, error) {
	return sandbox.Target{Key: h.targetName, Daemon: h.daemon}, nil
}

func (h *fakeHost) Resolve(_ context.Context, ref string) (sandbox.Image, error) {
	if id, ok := h.refs[ref]; ok {
		return sandbox.Image{ID: id, Name: ref, Digest: h.digests[ref]}, nil
	}
	for _, id := range h.refs {
		if id == ref {
			return sandbox.Image{ID: id, Name: ref}, nil
		}
	}
	return sandbox.Image{}, fmt.Errorf("%w: %s", sandbox.ErrNoImage, ref)
}

// Stamps answers for the clis asked: with nothing set, an image that holds
// each at the version pinned for it; else what stamped says it holds.
func (h *fakeHost) Stamps(_ context.Context, _ string, clis []string) (map[string]string, error) {
	if h.stampErr != nil {
		return nil, h.stampErr
	}
	out := map[string]string{}
	pins := images.ComponentPins()
	for _, c := range clis {
		switch v, ok := h.stamped[c]; {
		case ok:
			out[c] = v
		case h.stamped == nil && pins[c] != "":
			out[c] = pins[c]
		case h.stamped == nil:
			out[c] = "0.0.0"
		}
	}
	return out, nil
}

func (h *fakeHost) Images(context.Context) (map[string]bool, error) {
	ids := map[string]bool{}
	for _, id := range h.refs {
		ids[id] = true
	}
	for id := range h.untagged {
		ids[id] = true
	}
	return ids, nil
}

func (h *fakeHost) Pull(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error) {
	if _, ok := h.refs[ref]; ok && !force {
		img, err := h.Resolve(ctx, ref)
		return img, false, err
	}
	if log != nil {
		fmt.Fprintf(log, "pulled %s\n", ref)
	}
	h.pulls = append(h.pulls, ref)
	if !h.samePull || h.refs[ref] == "" {
		h.refs[ref] = "sha256:pulled-" + ref
		if h.pullTo != "" {
			h.refs[ref] = h.pullTo
		}
	}
	img, err := h.Resolve(ctx, ref)
	return img, err == nil, err
}

func (h *fakeHost) ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error) {
	return h.Pull(ctx, ref, force, log)
}

// Build builds every spec into an image named after its base, or its
// dockerfile.
func (h *fakeHost) Build(_ context.Context, spec sandbox.BuildSpec, log io.Writer) (sandbox.Image, string, bool, error) {
	id := "sha256:built-" + spec.Base + spec.Dockerfile
	h.refs[id] = id
	return sandbox.Image{ID: id}, "", true, nil
}

func (h *fakeHost) Upstream(_ context.Context, ref string) (string, error) {
	if h.upstream == nil {
		return "", errors.New("offline")
	}
	return h.upstream(ref)
}

func (h *fakeHost) CPUs(context.Context) int { return h.cpus }

var errNoImage = errors.New("no such image")

// useHost makes every sandbox's image host h for the test.
func useHost(t *testing.T, h host) {
	t.Helper()
	old := hostFor
	hostFor = func(config.Sandbox) (host, error) { return h, nil }
	t.Cleanup(func() { hostFor = old })
}

// pinAll pins every sandbox of cfg on a fake host holding exactly those
// images: a sandbox's configured image (img-<id> when it names none) and,
// under egress, proxy-<id>. a run then starts each sandbox from its
// configured image, as the run tests' fakes expect.
func pinAll(t *testing.T, dir string, cfg *config.Config) *fakeHost {
	t.Helper()
	h := newFakeHost()
	useHost(t, h)
	require.NoError(t, updatePins(dir, func(f *pinFile) error {
		for _, sb := range cfg.Sandboxes {
			id := sb.Image
			if id == "" {
				id = "img-" + sb.Name
			}
			h.refs[id] = id
			in, err := localInputs(sb, dir)
			require.NoError(t, err)
			// the image holds what the sandbox's agents run, at the pinned versions.
			comps := map[string]string{}
			for _, c := range sandboxCLIs(cfg, sb.Name) {
				comps[c] = cliPins(cfg)[c]
			}
			e := &pinEntry{Daemon: h.daemon, Image: pinImage{ID: id}, Inputs: in, Components: comps,
				PatsVersion: version.Info.Version, PatsBuild: patsBuild()}
			if sb.Egress.Set {
				h.refs["proxy-"+sb.Name] = "proxy-" + sb.Name
				e.Proxy = &pinImage{ID: "proxy-" + sb.Name}
			}
			f.setPin(h.targetName, sb.Name, e, time.Now())
		}
		return nil
	}))
	return h
}

// useFakeSandbox makes every run's sandbox fake for the test.
func useFakeSandbox(t *testing.T, fake sandbox.Sandbox) {
	t.Helper()
	old := newSandbox
	newSandbox = func(sandbox.Options, sandbox.SandboxOptions) (sandbox.Sandbox, error) { return fake, nil }
	t.Cleanup(func() { newSandbox = old })
}

// FuzzReadPins: .pats/images.json is read on every init and run, and may be
// hand-edited, cut short or another pats's. whatever it holds, reading it is
// an error (init then remakes it) or a pin file init can go on with: written
// back it reads the same, and pruning it does not panic.
func FuzzReadPins(f *testing.F) {
	hex := strings.Repeat("ab", 32)
	for _, s := range []string{
		`{"version":1}`, `{"version":2}`, `{}`, `[]`, `{"version":1,"superseded":[]}`, `{"version":1,"docker:local":{}}`,
		`{"version":1,"docker:local":{"s":null}}`, `{"version":1,"lxd:srv":{"s":null}}`, `{"version":1,"docker:local":null}`,
		`{"version":1,"superseded":[{"target":"podman:local","id":"` + hex + `","built":true,"since":"2026-11-01T10:00:00Z"}],` +
			`"podman:local":{"sxx":{"daemon":"d","image":{"id":"` + hex + `","name":"img","pulled":true},"layers":["sha256:l1"],` +
			`"proxy":{"id":"` + hex + `"},"pats_version":"1.0.0","pats_build":"dev","built":"2026-11-01","inputs":{"image_ref":"img"},` +
			`"components":{"claude":"2.1.0"}}},` +
			`"lxd:srv":{"lxx":{"image":{"id":"` + hex + `","built":true},"proxy":null,"pats_version":"1.0.0","pats_build":"dev","built":"","inputs":null}}}`,
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir() // one config dir for every input
	path := filepath.Join(dir, ".pats", pinsName)
	require.NoError(f, os.MkdirAll(filepath.Dir(path), 0o755))
	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(path, data, 0o644))
		pins, err := readPins(dir)
		if err != nil {
			return
		}

		written, err := json.Marshal(pins)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, written, 0o644))
		back, err := readPins(dir)
		require.NoError(t, err, "its own writing does not read back: %s", written)
		again, err := json.Marshal(back)
		require.NoError(t, err)
		assert.Equal(t, string(again), string(written))

		// what an init does next with a sandbox the config dropped.
		pins.dropMissing(nil, time.Unix(0, 0))
	})
}

// a pin file that is not one is an error to a run and is remade by an init;
// one that cannot be read at all is neither replaced nor taken for empty,
// since the images it lists as pats's own would never be removed.
func TestPinsUnreadable(t *testing.T) {
	write := func(t *testing.T, body string) (dir, path string) {
		dir = t.TempDir()
		path = filepath.Join(dir, ".pats", pinsName)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		return dir, path
	}
	pin := func(f *pinFile) error {
		f.setPin("docker:local", "sxx", &pinEntry{Image: pinImage{ID: "sha256:aa", Pulled: true}}, time.Unix(0, 0))
		return nil
	}
	for name, body := range map[string]string{
		"cut short":            `{"version":1,"docker:local":{"sxx":`,
		"another format":       `{"version":2}`,
		"a null container pin": `{"version":1,"docker:local":{"sxx":null}}`,
		"a null lxd pin":       `{"version":1,"lxd:srv":{"sxx":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := write(t, body)
			_, err := readPins(dir)
			assert.Error(t, err, "pats init")
			require.NoError(t, updatePins(dir, pin))
			back, err := readPins(dir)
			require.NoError(t, err)
			assert.Equal(t, back.entry("docker:local", "sxx").Image.ID, "sha256:aa")
		})
	}
	t.Run("not readable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads every file")
		}
		const body = `{"version":1,"superseded":[{"target":"docker:local","id":"sha256:bb","built":true,"since":"2026-11-01T10:00:00Z"}]}`
		dir, path := write(t, body)
		require.NoError(t, os.Chmod(path, 0))
		assert.Error(t, updatePins(dir, pin), "permission denied")
		require.NoError(t, os.Chmod(path, 0o644))
		kept, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, string(kept), body)
	})
}

// a container pin podman's inspect wrote holds a bare hex id; read back it is
// the driver's sha256:<hex>, so a re-init does not take it for another
// image. an lxd fingerprint stays as it is.
func TestPinsNormalizeIDs(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{
		"docker:default": {"s": {Image: pinImage{ID: hex}, Proxy: &pinImage{ID: hex}}},
		"lxd:srv":        {"l": {Image: pinImage{ID: hex}}},
	}, Superseded: []superseded{{Target: "docker:default", ID: hex}, {Target: "lxd:srv", ID: hex}}}
	f.normalizeIDs()
	require.Equal(t, f.Targets["docker:default"]["s"].Image.ID, "sha256:"+hex)
	require.Equal(t, f.Targets["docker:default"]["s"].Proxy.ID, "sha256:"+hex)
	require.Equal(t, f.Targets["lxd:srv"]["l"].Image.ID, hex)
	require.Equal(t, f.Superseded[0].ID, "sha256:"+hex)
	require.Equal(t, f.Superseded[1].ID, hex)
}
