//go:build !nobwrap

package eval

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// treeHost is a fake host that keeps trees beside its images, as bwrap does.
type treeHost struct {
	*fakeHost
	trees       map[string]bool
	stampedOver [][]string
}

func (h *treeHost) PlaceTrees(_ context.Context, placed []sandbox.Placed) ([]string, error) {
	var ids []string
	for _, p := range placed {
		h.trees[p.Artefact.TreeID()] = true
		ids = append(ids, p.Artefact.TreeID())
	}
	return ids, nil
}

func (h *treeHost) MissingTree(ids []string) (string, bool) {
	for _, id := range ids {
		if !h.trees[id] {
			return id, true
		}
	}
	return "", false
}

func (h *treeHost) StampsOver(ctx context.Context, id string, trees, clis []string) (map[string]string, error) {
	h.stampedOver = append(h.stampedOver, trees)
	return h.fakeHost.Stamps(ctx, id, clis)
}

// a place that builds no image keeps each cli's tree beside the one it pulls:
// init builds nothing, pins the pulled image and what was placed, and stamps
// a session of the two. a run hands the pinned trees to its sandbox, and one
// that is gone is a pin to make again.
func TestInitKeepsTreesBesideAnImage(t *testing.T) {
	dir := t.TempDir()
	const body = `
sbx: {kind: sandbox, items: {bw: {driver: bwrap}}}
agx: {kind: agent, defaults: {model: m}, items: {oc: {harness: opencode-openrouter, sandbox: bw}}}
txx: {kind: task, items: {txx: {prompt: p, step-sandbox: bw, collect: "true"}}}
sux: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`
	cfg := parseCfg(t, body)
	h := &treeHost{fakeHost: newFakeHost(), trees: map[string]bool{}}
	useHost(t, h)
	var got []sandbox.SandboxOptions // the agent's sandbox, and each step's
	old := newSandbox
	newSandbox = func(_ sandbox.Options, o sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		got = append(got, o)
		return &fakeSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = old })

	var out bytes.Buffer
	require.NoError(t, Init(cfg, InitOptions{ConfigDir: dir, Out: &out}), "%s", out.String())
	assert.Len(t, h.builds, 0)
	pub, _ := sandbox.SandboxImage("")
	f, err := readPins(dir)
	require.NoError(t, err)
	e := f.entry("fake:host", "bw")
	require.NotNil(t, e)
	assert.Equal(t, e.Image, pinImage{ID: "sha256:pulled-" + pub, Name: pub, Pulled: true})
	tree := e.Harnesses["opencode"]["tree"]
	assert.That(t, h.trees[tree], "the pinned tree %q was not placed", tree)
	require.Len(t, h.stampedOver, 1)
	assert.EqualArrays(t, h.stampedOver[0], []string{tree})
	assert.That(t, e.Components["opencode"] != "", "no stamp for opencode: %v", e.Components)

	_, err = Run(parseCfg(t, body), Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, "%s", out.String())
	// preflight's and the agent's are two: the task's step makes the rest.
	require.That(t, len(got) > 2, "the run made %d sandbox(es): none for the task's step", len(got))
	for _, o := range got {
		assert.Equal(t, o.Image, "sha256:pulled-"+pub)
		assert.EqualArrays(t, o.Trees, []string{tree})
	}

	delete(h.trees, tree)
	_, err = Run(parseCfg(t, body), Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assert.Error(t, err, "a harness tree it was pinned with")
	assert.Error(t, err, "run `pats init` first")
}
