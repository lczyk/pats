package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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

func (h *fakeHost) Stamps(context.Context, string) (map[string]string, error) {
	if h.stampErr != nil {
		return nil, h.stampErr
	}
	out := map[string]string{}
	for k, v := range h.stamped {
		out[k] = v
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
				id = "img-" + sb.ID
			}
			h.refs[id] = id
			in, err := localInputs(sb, dir)
			require.NoError(t, err)
			e := &pinEntry{Daemon: h.daemon, Image: pinImage{ID: id}, Inputs: in,
				PatsVersion: version.Info.Version, PatsBuild: patsBuild()}
			if sb.Egress.Set {
				h.refs["proxy-"+sb.ID] = "proxy-" + sb.ID
				e.Proxy = &pinImage{ID: "proxy-" + sb.ID}
			}
			f.setPin(h.targetName, sb.ID, e, time.Now())
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

// a container pin podman's inspect wrote holds a bare hex id; read back it is
// the driver's sha256:<hex>, so a re-init does not take it for another
// image. an lxd fingerprint stays as it is.
func TestPinsNormalizeIDs(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	f := &pinFile{Version: pinsVersion, Targets: map[string]map[string]*pinEntry{
		"podman:local": {"s": {Image: pinImage{ID: hex}, Proxy: &pinImage{ID: hex}}},
		"lxd:srv":      {"l": {Image: pinImage{ID: hex}}},
	}, Superseded: []superseded{{Target: "podman:local", ID: hex}, {Target: "lxd:srv", ID: hex}}}
	f.normalizeIDs()
	require.Equal(t, f.Targets["podman:local"]["s"].Image.ID, "sha256:"+hex)
	require.Equal(t, f.Targets["podman:local"]["s"].Proxy.ID, "sha256:"+hex)
	require.Equal(t, f.Targets["lxd:srv"]["l"].Image.ID, hex)
	require.Equal(t, f.Superseded[0].ID, "sha256:"+hex)
	require.Equal(t, f.Superseded[1].ID, hex)
}
