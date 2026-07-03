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

func listTestConfig() *config.Config {
	return &config.Config{
		Sandboxes: []config.Sandbox{
			{ID: "box", Kind: "container", Image: "ubuntu", Egress: config.Egress{Mode: "proxy"}},
			{ID: "bare", Kind: "bwrap"}, // egress mode empty -> "open"
		},
		Agents: []config.Agent{
			{ID: "claude", Kind: "claude-cli-keyless", Model: "opus", Sandbox: "box", Effort: "high"},
		},
		Tasks: []config.Task{{ID: "refactor", Prompt: "refactor.txt"}},
		Scorers: []config.Scorer{
			{ID: "exact", Score: "exact.sh"}, // default kind -> exec
			{ID: "judge", Kind: "agent", AgentID: "claude"},
		},
	}
}

func TestListAgents(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListAgents(listTestConfig(), &out))
	s := out.String()
	for _, want := range []string{"claude", "opus", "box", "high"} {
		assert.ContainsString(t, s, want)
	}
}

func TestListTasks(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListTasks(listTestConfig(), &out))
	assert.ContainsString(t, out.String(), "refactor")
	assert.ContainsString(t, out.String(), "refactor.txt")
}

func TestListSandboxes(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListSandboxes(listTestConfig(), &out))
	s := out.String()
	for _, want := range []string{"proxy", "docker", "bare", "open"} { // docker = resolved driver, open = default egress
		assert.ContainsString(t, s, want)
	}
}

func TestListScorers(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListScorers(listTestConfig(), &out))
	s := out.String()
	assert.ContainsString(t, s, "exact.sh") // exec -> file
	assert.ContainsString(t, s, "judge")    // agent scorer id
	assert.ContainsString(t, s, "agent")    // agent -> agent-id source
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
	require.NoError(t, ListSuites(listTestConfig(), &out))
	assert.ContainsString(t, out.String(), "SUITE")
}
