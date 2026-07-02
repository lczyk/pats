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
	// (2 + 1 scorers) each x "*" (3 tasks) = 9 pairs. (the agent scorer is
	// commented out until agent scorers are implemented.)
	assert.Len(t, score, 9)
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
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: a, kind: bogus, model: m, sandbox: s}
`,
			want: "unknown kind",
		},
		{
			name: "agent missing model",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: a, kind: opencode-openrouter, sandbox: s}
`,
			want: "model is required",
		},
		{
			name: "agent missing sandbox, many defined",
			src: `
sandboxes:
  - {id: a, kind: container, image: img}
  - {id: b, kind: container, image: img}
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
  - {id: sc, kind: agent, agent-id: ghost, prompt: p.txt}
`,
			want: "unknown agent-id",
		},
		{
			name: "container with image and build",
			src: `
sandboxes: [{id: s, kind: container, image: img, build: .}]
`,
			want: "mutually exclusive",
		},
		{
			name: "container with neither image nor build",
			src: `
sandboxes: [{id: s, kind: container}]
`,
			want: "needs an image or a build context",
		},
		{
			name: "bwrap not implemented",
			src: `
sandboxes: [{id: s, kind: bwrap}]
`,
			want: "bwrap kind not implemented",
		},
		{
			name: "agent scorer not implemented",
			src: `
scorers:
  - {id: sc, kind: agent, agent-id: ghost, prompt: p.txt}
`,
			want: "agent kind not implemented",
		},
		{
			// both real kinds take an effort flag now (claude --effort, opencode
			// --variant), so only an unknown kind can trip this today; the check
			// stays for future effort-less kinds.
			name: "effort on a kind without it",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: a, kind: bogus, model: m, sandbox: s, effort: high}
`,
			want: "effort is not supported",
		},
		{
			name: "egress mode off renamed",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: off}}]
`,
			want: "renamed",
		},
		{
			name: "unknown egress mode",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: firewall}}]
`,
			want: "unknown egress mode",
		},
		{
			name: "duplicate test pair",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
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
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: h1, kind: claude-cli-keyless, model: m, sandbox: s}
  - {id: h2, kind: opencode-openrouter, model: m, sandbox: s}
tasks:
  - {id: t1, prompt: p.txt}
  - {id: t2, prompt: p.txt}
test-matrix:
  - {agent: "*", task: "*"}
`)
	pairs, err := c.ExpandTestMatrix()
	require.NoError(t, err)
	// "*" agents = 2 x 2 tasks = 4.
	assert.Len(t, pairs, 4)
}

func TestScorerExecFile(t *testing.T) {
	// ${id} in the score path is replaced by the scorer id.
	assert.Equal(t, Scorer{ID: "x", Score: "scorers/${id}.py"}.ExecFile(), "scorers/x.py")
	// no ${id} -> path used verbatim.
	assert.Equal(t, Scorer{ID: "x", Score: "custom.py"}.ExecFile(), "custom.py")
	// no score -> empty.
	assert.Equal(t, Scorer{ID: "x"}.ExecFile(), "")
}
