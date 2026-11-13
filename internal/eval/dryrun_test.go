package eval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// writeMeta writes a past pair's metadata under run.
func writeMeta(t *testing.T, dir, run string, m pairMeta) {
	p := filepath.Join(dir, runsSubdir, run, m.Agent, m.Task, "metadata.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	b, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, b, 0o644))
}

func costMeta(agent, task, model string, cost float64, has bool) pairMeta {
	return pairMeta{Agent: agent, Task: task, Harness: "opencode-openrouter", Model: model, Status: "ok",
		Summary: &runSummary{CostUSD: cost, HasCost: has}}
}

// the dry run lists each pair with the median cost of the same pair before
// (same agent, task, harness, model and effort, with a cost), makes no run
// dir and starts no sandbox; --json says the same as json lines.
func TestDryRun(t *testing.T) {
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		t.Fatal("the dry run started a sandbox")
		return nil, nil
	}
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
avx: {kind: agent, items: {orx: {harness: opencode-openrouter, model: m, sandbox: sxx}, cxx: {harness: codex-cli-keyless, model: g, sandbox: sxx}}}
tvx: {kind: task, items: {txx: {prompt: p}, uxx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: avx, tasks: tvx}}}
`)
	pinAll(t, dir, cfg)
	writeMeta(t, dir, "001-20261101-a-b", costMeta("orx", "txx", "m", 1, true))
	writeMeta(t, dir, "002-20261102-c-d", costMeta("orx", "txx", "m", 3, true))
	writeMeta(t, dir, "003-20261103-e-f", costMeta("orx", "txx", "m", 2, true))
	writeMeta(t, dir, "003-20261103-e-f", costMeta("orx", "uxx", "m", 9, true))              // another task
	writeMeta(t, dir, "004-20261104-g-h", costMeta("orx", "txx", "other", 50, true))         // another model
	writeMeta(t, dir, "005-20261105-i-j", costMeta("orx", "txx", "m", 70, false))            // no cost
	bad := filepath.Join(dir, runsSubdir, "006-20261106-k-l", "orx", "uxx", "metadata.json") // outside the selection
	require.NoError(t, os.MkdirAll(filepath.Dir(bad), 0o755))
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o644))
	require.NoError(t, os.Symlink("003-20261103-e-f", filepath.Join(dir, runsSubdir, "latest")))
	runs, _ := os.ReadDir(filepath.Join(dir, runsSubdir))

	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, DryRun: true, Tasks: []string{"txx"}})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), "INFO orx/txx  sandbox sxx  est $2.00 (median of 3)\n")
	assert.ContainsString(t, out.String(), "INFO cxx/txx  sandbox sxx  est unknown (no history)\n")
	assert.ContainsString(t, out.String(), "INFO 2 pair(s), est $2.00 + 1 unknown\n")
	assert.ContainsString(t, out.String(), "WARN skipping ")
	after, _ := os.ReadDir(filepath.Join(dir, runsSubdir))
	assert.Equal(t, len(after), len(runs)) // no run dir made

	var js, log bytes.Buffer
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &log, DryRun: true, JSON: true, JSONOut: &js, Tasks: []string{"txx"}, Repeat: 5, Budget: 20})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(js.String()), "\n")
	require.Len(t, lines, 3)
	assert.ContainsString(t, lines[0], `"agent":"orx","task":"txx","sandbox":"sxx","estimate_usd":2,"samples":3,"unknown":null,"repeat":5,"estimate_total_usd":10`)
	assert.ContainsString(t, lines[1], `"unknown":"no history"`)
	assert.Equal(t, lines[2], `{"total":true,"pairs":2,"executions":10,"known_usd":10,"unknown_pairs":1,"budget_usd":20,"fits":true}`)
	assert.ContainsString(t, log.String(), "WARN skipping ") // warnings go to the log, never into the json
}

// a project with no runs yet estimates every pair as unknown.
func TestDryRunNoHistory(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {orx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, DryRun: true})
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), "est unknown (no history)")
}

// a history file that cannot be read stops the dry run with its path.
func TestDryRunUnreadableHistory(t *testing.T) {
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {orx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	writeMeta(t, dir, "001-20261101-a-b", costMeta("x", "y", "m", 1, true)) // another pair
	p := filepath.Join(dir, runsSubdir, "001-20261101-a-b", "x", "y", "metadata.json")
	require.NoError(t, os.Chmod(p, 0))
	defer os.Chmod(p, 0o644)
	var out bytes.Buffer
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, DryRun: true})
	assert.Error(t, err, p)
}
