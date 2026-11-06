package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
)

// listTestConfig is parsed, not validated: it holds a bwrap sandbox and an
// agent scorer, both rejected by Validate, so list output covers them. stock
// names neither image nor build, the published pats sandbox.
func listTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
boxes:
  kind: sandbox
  items:
    box: {image: ubuntu, egress: {allow: [a.test]}}
    bare: {driver: bwrap}
    stock: {}
agents:
  kind: agent
  items:
    claude: {harness: claude-cli-keyless, model: opus, sandbox: box, effort: high}
tasks:
  kind: task
  items:
    refactor: {prompt-file: refactor.txt}
scorers:
  kind: scorer
  items:
    exact: {score-cmd: exact.sh}
    judge: {agent: claude, prompt: judge.txt}
`))
	require.NoError(t, err)
	return cfg
}

func TestListAgents(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListAgents(listTestConfig(t), &out))
	s := out.String()
	for _, want := range []string{"claude", "opus", "box", "high"} {
		assert.ContainsString(t, s, want)
	}
}

func TestListTasks(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListTasks(listTestConfig(t), &out))
	assert.ContainsString(t, out.String(), "refactor")
	assert.ContainsString(t, out.String(), "refactor.txt")
}

func TestListSandboxes(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListSandboxes(listTestConfig(t), &out))
	s := out.String()
	for _, want := range []string{"proxy", "docker", "bare", "open", "ghcr.io/lczyk/pats/sandbox:"} { // docker = resolved driver, open = default egress
		assert.ContainsString(t, s, want)
	}
}

func TestListScorers(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListScorers(listTestConfig(t), &out))
	s := out.String()
	assert.ContainsString(t, s, "exact.sh")      // file scorer -> its script
	assert.ContainsString(t, s, "agent: claude") // agent scorer -> its judge
}

func TestListRuns(t *testing.T) {
	dir := t.TempDir()
	mk := func(run, task, status string) {
		od := filepath.Join(dir, runsSubdir, run, "a", task)
		require.NoError(t, os.MkdirAll(od, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(od, "metadata.json"),
			[]byte(`{"status":"`+status+`"}`), 0o644))
	}
	mk("20260101-001", "t1", "ok")
	mk("20260101-001", "t2", "ok")
	mk("20260101-002", "t1", "ok")
	mk("20260101-002", "t2", "error")
	require.NoError(t, os.WriteFile(filepath.Join(dir, runsSubdir, "20260101-001", "scores.json"),
		[]byte(`{"overall":0.5}`), 0o644))

	var out bytes.Buffer
	require.NoError(t, ListRuns(dir, &out))
	s := out.String()
	assert.ContainsString(t, s, "20260101-001")
	assert.ContainsString(t, s, "2 ok")          // tally
	assert.ContainsString(t, s, "0.50")          // scored
	assert.ContainsString(t, s, "1 ok, 1 error") // mixed-status tally, ordered
}

// no runs dir -> no error, empty-ish output.
func TestListRunsEmpty(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListRuns(t.TempDir(), &out))
}

func TestListSuites(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListSuites(listTestConfig(t), &out))
	assert.ContainsString(t, out.String(), "SUITE")
}
