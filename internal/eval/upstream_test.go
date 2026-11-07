package eval

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

func fakeLookups(t *testing.T, registry func(ctx context.Context, ref string) (string, error), lxd func(ctx context.Context, o sandbox.Options, ref string) (string, error)) {
	t.Helper()
	oldR, oldL := registryDigest, lxdRemoteFingerprint
	registryDigest, lxdRemoteFingerprint = registry, lxd
	t.Cleanup(func() { registryDigest, lxdRemoteFingerprint = oldR, oldL })
}

// a moved upstream ref warns; an unmoved one, a failed lookup and one past
// the budget say nothing; the whole check ends at the budget.
func TestCheckUpstream(t *testing.T) {
	fakeLookups(t,
		func(ctx context.Context, ref string) (string, error) {
			switch ref {
			case "moved:1":
				return "sha256:new", nil
			case "broken:1":
				return "", errors.New("401")
			case "slow:1":
				<-ctx.Done()
				return "sha256:late", nil
			}
			return "sha256:same", nil
		},
		func(ctx context.Context, o sandbox.Options, ref string) (string, error) { return "fp-new", nil })
	checks := []upstreamCheck{
		{sb: "c", inputs: map[string]string{
			"upstream:moved:1": "sha256:old", "upstream:same:1": "sha256:same",
			"upstream:broken:1": "sha256:x", "upstream:slow:1": "sha256:x", "provision": "h"}},
		{sb: "l", lxd: true, inputs: map[string]string{"upstream:ubuntu:26.04": "fp-old"}},
	}
	start := time.Now()
	warns := checkUpstream(context.Background(), checks, 200*time.Millisecond)
	assert.That(t, time.Since(start) < 2*time.Second, "the check outran its budget")
	require.Equal(t, len(warns), 2)
	assert.ContainsString(t, warns[0], `sandbox "c": a newer moved:1 is upstream`)
	assert.ContainsString(t, warns[1], `sandbox "l": a newer ubuntu:26.04 is upstream`)
}

// init records what each sandbox came from upstream: an image: sandbox's
// digest, a build's FROM images (not pats's own), an lxd base's fingerprint.
func TestUpstreamInputs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ctx"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ctx", "Dockerfile"),
		[]byte("ARG PATS_SANDBOX\nFROM python:3 AS b\nFROM ${PATS_SANDBOX}\nCOPY --from=b / /\n"), 0o644))
	fakeLookups(t, nil, func(ctx context.Context, o sandbox.Options, ref string) (string, error) { return "fp-" + ref, nil })
	h := newFakeHost()
	h.refs["python:3"] = "sha256:py"
	h.digests = map[string]string{"python:3": "sha256:pydigest"}
	cfg := parseCfg(t, `
sb:
  kind: sandbox
  items:
    built: {build: ctx}
    img: {image: my/image:1}
    box: {driver: lxd}
ag: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {a: {sandbox: built}, b: {sandbox: img}, c: {sandbox: box}}}
t: {kind: task, items: {t: {prompt: p}}}
suites: {kind: suite, items: {all: {agents: ag, tasks: t}}}
`)
	ctx := context.Background()
	var log bytes.Buffer
	// a build's bases, before it runs: present, or pulled first.
	assert.EqualMaps(t, baseDigests(ctx, cfg.Sandboxes[0], dir, h, &log), map[string]string{"python:3": "sha256:pydigest"})
	delete(h.refs, "python:3")
	h.digests["python:3"] = "sha256:pulled"
	assert.EqualMaps(t, baseDigests(ctx, cfg.Sandboxes[0], dir, h, &log), map[string]string{"python:3": "sha256:pulled"})
	published, _ := sandbox.SandboxImage("")
	assert.EqualArraysUnordered(t, h.pulls, []string{published, "python:3"}) // FROM ${PATS_SANDBOX} too
	e := &pinEntry{Inputs: map[string]string{}}
	upstreamInputs(ctx, cfg.Sandboxes[0], dir, h, e)
	assert.Len(t, e.Inputs, 0) // a built image's bases come from baseDigests
	e = &pinEntry{Inputs: map[string]string{}, Image: pinImage{Name: "my/image:1", Digest: "sha256:mine"}}
	upstreamInputs(ctx, cfg.Sandboxes[1], dir, h, e)
	assert.EqualMaps(t, e.Inputs, map[string]string{"upstream:my/image:1": "sha256:mine"})
	e = &pinEntry{Inputs: map[string]string{}, Image: pinImage{ID: "fp-copied", Name: "ubuntu:24.04"}}
	box := cfg.Sandboxes[2]
	box.Image = "ubuntu:24.04"
	upstreamInputs(ctx, box, dir, h, e)
	assert.EqualMaps(t, e.Inputs, map[string]string{"upstream:ubuntu:24.04": "fp-copied"})

	// the default sandbox on a dev build: :latest, which moves, is recorded;
	// a release's own tag is not.
	e = &pinEntry{Inputs: map[string]string{}, Image: pinImage{Name: "ghcr.io/lczyk/pats/sandbox:latest", Digest: "sha256:l"}}
	upstreamInputs(ctx, config.Sandbox{ID: "d"}, dir, h, e)
	assert.EqualMaps(t, e.Inputs, map[string]string{"upstream:ghcr.io/lczyk/pats/sandbox:latest": "sha256:l"})
	e = &pinEntry{Inputs: map[string]string{}, Image: pinImage{Name: "ghcr.io/lczyk/pats/sandbox:v1.0.0", Digest: "sha256:r"}}
	upstreamInputs(ctx, config.Sandbox{ID: "d"}, dir, h, e)
	assert.Len(t, e.Inputs, 0)
}

// a run checks upstream while preflight runs and warns; --offline skips it.
func TestRunUpstreamWarnsAndOffline(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, initCfg)
	pinAll(t, dir, cfg)
	require.NoError(t, updatePins(dir, func(f *pinFile) error {
		f.Targets["fake:host"]["own"].Inputs["upstream:my/image:1"] = "sha256:old"
		return nil
	}))
	var calls atomic.Int32
	fakeLookups(t, func(context.Context, string) (string, error) { calls.Add(1); return "sha256:new", nil }, nil)
	useFakeSandbox(t, &fakeSandbox{})
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), `sandbox "own": a newer my/image:1 is upstream`)

	calls.Store(0)
	out.Reset()
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-own"}, Offline: true})
	require.NoError(t, err)
	assert.Equal(t, calls.Load(), int32(0))
	assert.That(t, !bytes.Contains(out.Bytes(), []byte("upstream")), "--offline still checked: %s", out.String())
}

func TestPublishedRepo(t *testing.T) {
	assert.That(t, publishedRepo("ghcr.io/lczyk/pats/sandbox:v1.0.0-beta.5"), "a release tag is checked")
	assert.That(t, !publishedRepo("ghcr.io/lczyk/pats/sandbox:latest"), ":latest moves, and is not skipped")
	assert.That(t, !publishedRepo("ghcr.io/lczyk/pats/sandbox:26.04"), "the ubuntu tag moves, and is not skipped")
	assert.That(t, !publishedRepo("ubuntu:26.04"), "another image is not pats's")
}

// a pinned proxy that can move upstream is checked like the image.
func TestUpstreamProxy(t *testing.T) {
	e := &pinEntry{Inputs: map[string]string{}, Image: pinImage{Name: "my/image:1", Digest: "sha256:i"},
		Proxy: &pinImage{Name: "ghcr.io/example/proxy:latest", Digest: "sha256:p"}}
	upstreamInputs(context.Background(), config.Sandbox{Image: "my/image:1"}, t.TempDir(), newFakeHost(), e)
	assert.EqualMaps(t, e.Inputs, map[string]string{"upstream:my/image:1": "sha256:i", "upstream:ghcr.io/example/proxy:latest": "sha256:p"})
}

// a base whose tag moved across the build gets no baseline rather than the
// wrong one; pats's release tags get none either way.
func TestStableBases(t *testing.T) {
	held, moved := stableBases(
		map[string]string{"python:3": "sha256:a", "ubuntu:26.04": "sha256:u", "ghcr.io/lczyk/pats/sandbox:v1.0.0": "sha256:p"},
		map[string]string{"python:3": "sha256:b", "ubuntu:26.04": "sha256:u", "ghcr.io/lczyk/pats/sandbox:v1.0.0": "sha256:p"})
	assert.EqualMaps(t, held, map[string]string{"ubuntu:26.04": "sha256:u"})
	assert.EqualArrays(t, moved, []string{"python:3"})
}
