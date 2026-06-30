package eval

import (
	"bytes"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
)

func listTestConfig() *config.Config {
	return &config.Config{
		Sandboxes: []config.Sandbox{
			{ID: "box", Kind: "container", Image: "ubuntu", Egress: config.Egress{Mode: "proxy"}},
			{ID: "bare", Kind: "bwrap"}, // egress mode empty -> "off"
		},
		Agents: []config.Agent{
			{ID: "claude", Kind: "claude-cli-keyless", Model: "opus", Sandbox: "box", Effort: "high"},
		},
		Tasks: []config.Task{{ID: "refactor", PromptFile: "refactor.txt"}},
		Scorers: []config.Scorer{
			{ID: "exact", Kind: "bash", File: "exact.sh"},
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
	for _, want := range []string{"proxy", "docker", "bare", "off"} { // docker = resolved driver, off = default egress
		assert.ContainsString(t, s, want)
	}
}

func TestListScorers(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, ListScorers(listTestConfig(), &out))
	s := out.String()
	assert.ContainsString(t, s, "exact.sh") // bash -> file
	assert.ContainsString(t, s, "judge")    // agent scorer id
	assert.ContainsString(t, s, "agent")    // agent -> agent-id source
}
