package config

import (
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the committed example must load, validate, and expand to the matrices its
// comments describe -- a regression guard on both the parser and the example.
func TestExampleConfig(t *testing.T) {
	c, err := Load("../../pats.example.yaml")
	require.NoError(t, err)
	require.NoError(t, c.Validate())

	test, err := c.ExpandTestMatrix()
	require.NoError(t, err)
	// 2 agents x "*" (3 tasks) = 6 pairs.
	assert.Len(t, test, 6)

	score, err := c.ExpandScorerMatrix()
	require.NoError(t, err)
	// (2 + 1 + 1 scorers) each x "*" (3 tasks) = 12 pairs.
	assert.Len(t, score, 12)
}

func parseT(t *testing.T, src string) *Config {
	t.Helper()
	c, err := parse([]byte(src))
	require.NoError(t, err)
	return c
}

func TestStrListScalarOrList(t *testing.T) {
	c := parseT(t, `
test-matrix:
  - agent: solo
    task: [a, b]
`)
	assert.EqualArrays(t, []string(c.TestMatrix[0].Agent), []string{"solo"})
	assert.EqualArrays(t, []string(c.TestMatrix[0].Task), []string{"a", "b"})
}

func TestWeightDefault(t *testing.T) {
	c := parseT(t, `
agents:
  - {id: a, kind: opencode-openrouter, model: m, sandbox: s}
sandboxes:
  - {id: s, kind: bwrap}
tasks:
  - {id: t, prompt-file: p.txt}
test-matrix:
  - {agent: a, task: t}
  - {agent: a, task: t2, weight: 2.5}
`)
	assert.Equal(t, c.TestMatrix[0].WeightOr(1.0), 1.0)
	assert.Equal(t, c.TestMatrix[1].WeightOr(1.0), 2.5)
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := parse([]byte("agents:\n  - {id: a, kind: opencode-openrouter, bogus: 1}\n"))
	assert.Error(t, err, assert.AnyError)
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "unknown agent kind",
			src: `
sandboxes: [{id: s, kind: bwrap}]
agents:
  - {id: a, kind: bogus, model: m, sandbox: s}
`,
			want: "unknown kind",
		},
		{
			name: "agent missing model",
			src: `
sandboxes: [{id: s, kind: bwrap}]
agents:
  - {id: a, kind: opencode-openrouter, sandbox: s}
`,
			want: "model is required",
		},
		{
			name: "agent missing sandbox, many defined",
			src: `
sandboxes:
  - {id: a, kind: bwrap}
  - {id: b, kind: bwrap}
agents:
  - {id: h, kind: claude-cli-keyless, model: m}
`,
			want: "no sandbox set",
		},
		{
			name: "container needs image",
			src: `
sandboxes: [{id: s, kind: container, driver: docker}]
agents:
  - {id: h, kind: claude-cli-keyless, model: m, sandbox: s}
`,
			want: "needs an image",
		},
		{
			name: "dangling scorer agent-id",
			src: `
scorers:
  - {id: sc, kind: agent, agent-id: ghost, prompt-file: p.txt}
`,
			want: "unknown agent-id",
		},
		{
			name: "duplicate test pair",
			src: `
sandboxes: [{id: s, kind: bwrap}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt-file: p.txt}]
test-matrix:
  - {agent: a, task: t}
  - {agent: a, task: t}
`,
			want: "duplicate pair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.Error(t, c.Validate(), tc.want)
		})
	}
}

func TestWildcardResolution(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: bwrap}]
agents:
  - {id: h1, kind: claude-cli-keyless, model: m, sandbox: s}
  - {id: h2, kind: opencode-openrouter, model: m, sandbox: s}
tasks:
  - {id: t1, prompt-file: p.txt}
  - {id: t2, prompt-file: p.txt}
test-matrix:
  - {agent: "*", task: "*"}
`)
	pairs, err := c.ExpandTestMatrix()
	require.NoError(t, err)
	// "*" agents = 2 x 2 tasks = 4.
	assert.Len(t, pairs, 4)
}
