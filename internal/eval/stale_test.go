package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the local inputs follow the provision script, its bin/ and its base, and a
// build: context; nothing else.
func TestLocalInputs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "provision-box"), []byte("#!/bin/sh\ntrue\n"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ctx"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ctx", "Dockerfile"), []byte("FROM scratch\nCOPY x /x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ctx", "x"), []byte("1"), 0o644))
	cfg := parseCfg(t, `
sbx:
  kind: sandbox
  items:
    prov: {base: ubuntu:26.04, provision: provision-box}
    built: {build: ctx}
agx: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {axx: {sandbox: prov}, bxx: {sandbox: built}}}
txx: {kind: task, items: {txx: {prompt: p}}}
suites: {kind: suite, items: {all: {agents: agx, tasks: txx}}}
`)
	inputs := func(i int) map[string]string {
		t.Helper()
		in, err := localInputs(cfg.Sandboxes[i], dir)
		require.NoError(t, err)
		return in
	}
	prov, built := inputs(0), inputs(1)
	assert.EqualMaps(t, inputs(0), prov) // stable

	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "tool"), []byte("#!/bin/sh\necho\n"), 0o755))
	assert.That(t, inputs(0)["provision"] != prov["provision"], "a bin/ edit left the provision inputs as they were")
	prov = inputs(0)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "provision-box"), []byte("#!/bin/sh\nfalse\n"), 0o755))
	assert.That(t, inputs(0)["provision"] != prov["provision"], "a helper edit left the provision inputs as they were")
	prov = inputs(0)
	cfg.Sandboxes[0].Provision = "provision-box v2"
	assert.That(t, inputs(0)["provision"] != prov["provision"], "an argument change left the provision inputs as they were")
	cfg.Sandboxes[0].Base = "ubuntu:24.04"
	assert.Equal(t, inputs(0)["base_ref"], "ubuntu:24.04")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "ctx", "x"), []byte("2"), 0o644))
	assert.That(t, inputs(1)["context"] != built["context"], "a context edit left the build inputs as they were")
}

// a run warns, once per sandbox, about what changed since init, and goes on;
// a proxy from another pats stops it.
func TestRunStaleAndProxyMismatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, initCfg)
	pinAll(t, dir, cfg)
	fake := &fakeSandbox{}
	useFakeSandbox(t, fake)
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-pub", "a-own"}})
	require.NoError(t, err)
	assert.That(t, !bytes.Contains(out.Bytes(), []byte("changed since its pats init")), "a fresh pin warned: %s", out.String())

	require.NoError(t, updatePins(dir, func(f *pinFile) error {
		for _, e := range f.Targets["fake:host"] {
			e.PatsVersion = "0.0.1"
		}
		return nil
	}))
	out.Reset()
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-pub", "a-own"}})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), `sandbox "own": changed since its pats init: the pats version (0.0.1`)
	assert.ContainsString(t, out.String(), "pats init --sandbox own to rebuild")

	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Agents: []string{"a-proxied"}})
	assert.Error(t, err, "proxied: its egress proxy is from pats 0.0.1")
}
