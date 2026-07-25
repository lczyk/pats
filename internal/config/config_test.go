package config

import (
	"os"
	"strings"
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
			name: "file scorer with agent-id",
			src: `
scorers:
  - {id: sc, score: sc.py, agent-id: ghost}
`,
			want: "agent-id is an agent-scorer field",
		},
		{
			name: "file scorer with prompt",
			src: `
scorers:
  - {id: sc, score: sc.py, prompt: p.txt}
`,
			want: "prompt is an agent-scorer field",
		},
		{
			// every real kind takes an effort flag now (claude --effort, codex
			// model_reasoning_effort, opencode --variant), so only an unknown kind
			// can trip this today; the check stays for future effort-less kinds.
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
			name: "egress allow list without proxy or mitm-proxy mode",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: open, allow: ["good.com"]}}]
`,
			want: "allow/deny needs egress mode proxy or mitm-proxy",
		},
		{
			name: "egress deny list without proxy or mitm-proxy mode",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: none, deny: ["evil.com"]}}]
`,
			want: "allow/deny needs egress mode proxy or mitm-proxy",
		},
		{
			name: "egress allow list under default: allow (a denylist)",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, default: allow, allow: ["good.com"]}}]
`,
			want: "allow is ignored under default: allow",
		},
		{
			name: "egress deny list under default: deny (an allowlist)",
			src: `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, deny: ["evil.com"]}}]
`,
			want: "deny is ignored under default: deny",
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

// effortKinds is a subset view over AgentKinds; pin it so a typo'd or removed
// kind can't linger there.
func TestEffortKindsAreAgentKinds(t *testing.T) {
	for k := range effortKinds {
		assert.That(t, AgentKinds[k], "effortKinds entry %q is not an agent kind", k)
	}
}

func TestCodexAgentWithEffortValid(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: codex, kind: codex-cli-keyless, model: gpt-test, effort: high, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
suites: [{id: su, agents: codex, tasks: t}]
`)
	require.NoError(t, c.Validate())
}

func TestCopilotAgentWithEffortValid(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: container, image: img}]
agents: [{id: copilot, kind: copilot-cli-keyless, model: gpt-test, effort: high, sandbox: s}]
tasks: [{id: t, prompt: p.txt}]
suites: [{id: su, agents: copilot, tasks: t}]
`)
	require.NoError(t, c.Validate())
}

// nearby accepted case for the "egress allow/deny needs proxy" and "deny
// ignored under default: deny" rejections above: an allowlist used correctly
// (default: deny, only allow set) trips neither new check.
func TestEgressProxyAllowlistValid(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, default: deny, allow: ["good.com"]}}]
`)
	require.NoError(t, c.Validate())
}

// nearby accepted case for the "allow ignored under default: allow"
// rejection above: a denylist used correctly (default: allow, only deny set).
func TestEgressProxyDenylistValid(t *testing.T) {
	c := parseT(t, `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, default: allow, deny: ["evil.com"]}}]
`)
	require.NoError(t, c.Validate())
}

// nearby accepted case for the "file scorer with agent-id/prompt"
// rejections above: a plain file scorer setting neither field.
func TestFileScorerWithoutAgentFieldsValid(t *testing.T) {
	c := parseT(t, `
scorers:
  - {id: sc, score: sc.py}
`)
	require.NoError(t, c.Validate())
}

// COVER: validateEgress used to build the url-rule field/pattern pairs from a
// map, so a run with both deny-urls and allow-urls invalid joined its errors
// in random order. pin deny-urls before allow-urls across repeated parses.
func TestEgressURLRuleErrorOrderStable(t *testing.T) {
	src := `
sandboxes: [{id: s, kind: container, image: img, egress: {mode: proxy, deny-urls: ["a"], allow-urls: ["b"]}}]
`
	for range 20 {
		c := parseT(t, src)
		err := c.Validate()
		require.Error(t, err, assert.AnyError)
		msg := err.Error()
		denyIdx := strings.Index(msg, "deny-urls needs")
		allowIdx := strings.Index(msg, "allow-urls needs")
		assert.That(t, denyIdx >= 0 && allowIdx >= 0 && denyIdx < allowIdx,
			"expected deny-urls error before allow-urls, got: %s", msg)
	}
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
