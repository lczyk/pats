package config

import (
	"os"
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

	test, err := c.ExpandTestPairs()
	require.NoError(t, err)
	// 2 agents x 3 tasks = 6 pairs.
	assert.Len(t, test, 6)

	score, err := c.ExpandScorePairs()
	require.NoError(t, err)
	// 3 scorers x 3 tasks = 9 pairs. (the agent scorer is commented out until
	// agent scorers are implemented.)
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
suites:
  - id: s
    agents: solo
    tasks: [a, b]
`)
	assert.EqualArrays(t, []string(c.Suites[0].Agents), []string{"solo"})
	assert.EqualArrays(t, []string(c.Suites[0].Tasks), []string{"a", "b"})
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
			name: "deny-urls without mitm-proxy",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, deny-urls: ["github.com/x*"]}}]
`,
			want: "deny-urls needs egress mode mitm-proxy",
		},
		{
			name: "deny-urls wildcard host",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: mitm-proxy, deny-urls: ["*/chisel-releases*"]}}]
`,
			want: "literal hostname",
		},
		{
			name: "allow-urls without mitm-proxy",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, allow-urls: ["github.com/x*"]}}]
`,
			want: "allow-urls needs egress mode mitm-proxy",
		},
		{
			name: "duplicate id within a suite axis",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
suites:
  - {id: su, agents: [a, a], tasks: t}
`,
			want: "duplicate agent",
		},
		{
			name: "suite missing agents",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
suites:
  - {id: su, tasks: t, scorers: []}
`,
			want: "agents is required",
		},
		{
			name: "duplicate suite id",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
suites:
  - {id: su, agents: a, tasks: t}
  - {id: su, agents: a, tasks: t}
`,
			want: "duplicate suite id",
		},
		{
			name: "orphaned task",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks:
  - {id: t, prompt: p.txt}
  - {id: forgotten, prompt: p.txt}
suites:
  - {id: su, agents: a, tasks: t}
`,
			want: "task \"forgotten\" is in no suite",
		},
		{
			name: "orphaned scorer",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: a, kind: opencode-openrouter, model: m, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
scorers: [{id: sc, score: sc.py}]
suites:
  - {id: su, agents: a, tasks: t}
`,
			want: "scorer \"sc\" is in no suite",
		},
		{
			name: "orphaned agent",
			src: `
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: a, kind: opencode-openrouter, model: m, sandbox: s}
  - {id: idle, kind: opencode-openrouter, model: m, sandbox: s}
tasks: [{id: t, prompt: p.txt}]
suites:
  - {id: su, agents: a, tasks: t}
`,
			want: "agent \"idle\" is in no suite",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.Error(t, c.Validate(), tc.want)
		})
	}
}

// yaml anchors are the list-reuse mechanism now that wildcards are gone --
// pin that a shared anchor round-trips through StrList.
func TestYamlAnchorReuse(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: container, image: img}]
agents:
  - {id: h1, kind: claude-cli-keyless, model: m, sandbox: s}
  - {id: h2, kind: opencode-openrouter, model: m, sandbox: s}
tasks:
  - {id: t1, prompt: p.txt}
  - {id: t2, prompt: p.txt}
suites:
  - id: one
    agents: &all [h1, h2]
    tasks: [t1]
  - id: two
    agents: *all
    tasks: [t2]
`)
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	// 2 agents x t1 + 2 agents x t2 = 4.
	assert.Len(t, pairs, 4)
}

func TestAgentResolvedModel(t *testing.T) {
	// ${id} in the model is replaced by the agent id.
	assert.Equal(t, Agent{ID: "gpt-5-mini", Model: "openai/${id}"}.ResolvedModel(), "openai/gpt-5-mini")
	// no ${id} -> model used verbatim.
	assert.Equal(t, Agent{ID: "haiku", Model: "claude-haiku-4-5"}.ResolvedModel(), "claude-haiku-4-5")
}

func TestScorerExecFile(t *testing.T) {
	// ${id} in the score path is replaced by the scorer id.
	assert.Equal(t, Scorer{ID: "x", Score: "scorers/${id}.py"}.ExecFile(), "scorers/x.py")
	// no ${id} -> path used verbatim.
	assert.Equal(t, Scorer{ID: "x", Score: "custom.py"}.ExecFile(), "custom.py")
	// no score -> empty.
	assert.Equal(t, Scorer{ID: "x"}.ExecFile(), "")
}

// FuzzParse just wants parse() to never panic on arbitrary yaml.
func FuzzParse(f *testing.F) {
	seed, err := os.ReadFile("../../pats.example.yaml")
	if err == nil {
		f.Add(seed)
	}
	f.Add([]byte("agents: [foo\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parse(data)
	})
}
