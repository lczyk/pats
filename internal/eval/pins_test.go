package eval

import (
	"context"
	"fmt"
	"io"
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
}

func newFakeHost(refs ...string) *fakeHost {
	h := &fakeHost{targetName: "fake:host", daemon: "d", refs: map[string]string{}}
	for _, r := range refs {
		h.refs[r] = r
	}
	return h
}

func (h *fakeHost) target(context.Context) (string, string, error) {
	return h.targetName, h.daemon, nil
}

func (h *fakeHost) present(_ context.Context, ref string) bool {
	if _, ok := h.refs[ref]; ok {
		return true
	}
	for _, id := range h.refs {
		if id == ref {
			return true
		}
	}
	return false
}

func (h *fakeHost) stamps(context.Context, string) (map[string]string, error) {
	if h.stampErr != nil {
		return nil, h.stampErr
	}
	out := map[string]string{}
	for k, v := range h.stamped {
		out[k] = v
	}
	return out, nil
}

func (h *fakeHost) inspect(_ context.Context, ref string) (string, string, error) {
	if id, ok := h.refs[ref]; ok {
		return id, "", nil
	}
	return "", "", errNoImage
}

func (h *fakeHost) pull(_ context.Context, ref string, log io.Writer) error {
	fmt.Fprintf(log, "pulled %s\n", ref)
	h.pulls = append(h.pulls, ref)
	h.refs[ref] = "sha256:pulled-" + ref
	return nil
}

var errNoImage = &noImage{}

type noImage struct{}

func (*noImage) Error() string { return "no such image" }

// useHost makes every sandbox's image host h for the test.
func useHost(t *testing.T, h imageHost) {
	t.Helper()
	old := hostFor
	hostFor = func(config.Sandbox) imageHost { return h }
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
	newSandbox = func(sandbox.Options) (sandbox.Sandbox, error) { return fake, nil }
	t.Cleanup(func() { newSandbox = old })
}
