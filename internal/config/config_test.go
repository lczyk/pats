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
	c, err := Parse([]byte(src))
	require.NoError(t, err)
	return c
}

// base is a minimal valid config the error cases below extend.
const base = `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p.txt}}}
sc: {kind: scorer, items: {sc: {score: sc.py}}}
su: {kind: suite, items: {su: {agents: a, tasks: t, scorers: sc}}}
`

func TestParseVectors(t *testing.T) {
	c := parseT(t, `
slicer:
  kind: task
  defaults:
    prompt: prompts/${task}.txt
    prepare: tasks/${vector}/prepare_${task}.sh
    collect: tasks/collect.sh
  items:
    base-passwd:
    curl: {timeout: 15m, collect: ~}
misc:
  kind: task
  items:
    help: {prompt: help.txt}
`)
	require.Len(t, c.Tasks, 3)
	bp, curl, help := c.Tasks[0], c.Tasks[1], c.Tasks[2]

	// file order, vector stamped.
	assert.Equal(t, bp.ID, "base-passwd")
	assert.Equal(t, bp.Vector, "slicer")
	assert.Equal(t, help.Vector, "misc")

	// defaults inherited and interpolated per item.
	assert.Equal(t, bp.Prompt, "prompts/base-passwd.txt")
	assert.Equal(t, bp.Prepare, "tasks/slicer/prepare_base-passwd.sh")
	assert.Equal(t, bp.Collect, "tasks/collect.sh")
	assert.Equal(t, bp.Timeout, "")

	// item overrides a field, and an explicit null unsets one.
	assert.Equal(t, curl.Prompt, "prompts/curl.txt")
	assert.Equal(t, curl.Timeout, "15m")
	assert.Equal(t, curl.Collect, "")

	// a vector without defaults.
	assert.Equal(t, help.Prompt, "help.txt")
}

func TestParseSandboxDriverDefault(t *testing.T) {
	c := parseT(t, `
s:
  kind: sandbox
  items:
    d: {image: img}
    p: {driver: podman, image: img}
`)
	assert.Equal(t, c.Sandboxes[0].Driver, "docker")
	assert.Equal(t, c.Sandboxes[1].Driver, "podman")
}

// anchors + merge keys are how defaults are shared between vectors.
func TestParseDefaultsViaAnchors(t *testing.T) {
	c := parseT(t, `
sdf:
  kind: scorer
  defaults: &py {score: "scorers/${scorer}.py"}
  items: {one: ~, two: ~}
transcript:
  kind: scorer
  defaults: *py
  items: {three: ~}
misc:
  kind: task
  defaults: &td {prompt: p.txt, collect: c.sh}
  items: {x: ~}
misc2:
  kind: task
  defaults: {<<: *td, collect: ~, timeout: 5m}
  items: {y: ~, z: {collect: z.sh}}
`)
	assert.Equal(t, c.Scorers[2].Score, "scorers/three.py")
	assert.Equal(t, c.Scorers[2].Vector, "transcript")
	y, z := c.Tasks[1], c.Tasks[2]
	assert.Equal(t, y.Prompt, "p.txt")
	assert.Equal(t, y.Collect, "")
	assert.Equal(t, y.Timeout, "5m")
	assert.Equal(t, z.Collect, "z.sh")
}

func TestParseSuiteDefaults(t *testing.T) {
	c := parseT(t, `
suites:
  kind: suite
  defaults: {agents: all, scorers: [x, y]}
  items:
    one: {tasks: t1}
    two: {tasks: [t2, t3], scorers: z}
`)
	assert.EqualArrays(t, []string(c.Suites[0].Agents), []string{"all"})
	assert.EqualArrays(t, []string(c.Suites[0].Tasks), []string{"t1"})
	assert.EqualArrays(t, []string(c.Suites[0].Scorers), []string{"x", "y"})
	assert.EqualArrays(t, []string(c.Suites[1].Tasks), []string{"t2", "t3"})
	assert.EqualArrays(t, []string(c.Suites[1].Scorers), []string{"z"})
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"top level not a mapping", "- a\n", "top level must be a mapping"},
		{"vector not a mapping", "x: [1]\n", "vector x: must be a mapping"},
		{"missing kind", "x: {items: {a: ~}}\n", "vector \"x\": missing kind"},
		{"unknown kind", "x: {kind: tasks, items: {a: ~}}\n", "unknown kind \"tasks\""},
		{"unknown vector key", "x: {kind: task, item: {a:}}\n", "unknown key \"item\""},
		{"missing items", "x: {kind: task, defaults: {prompt: p}}\n", "items is required"},
		{"empty items", "x: {kind: task, items: {}}\n", "items is empty"},
		{"items not a mapping", "x: {kind: task, items: [a, b]}\n", "items: must be a mapping"},
		{"item not a mapping", "x: {kind: task, items: {a: [1]}}\n", "task \"a\": must be a mapping"},
		{"unknown field on item", "x: {kind: task, items: {a: {bogus: 1}}}\n", "task \"a\": unknown field \"bogus\""},
		{"unknown field in defaults", "x: {kind: agent, defaults: {kind: claude-cli-keyless}, items: {a: ~}}\n", "defaults: unknown field \"kind\""},
		{"unknown template variable", "x: {kind: task, items: {a: {prompt: \"p/${id}.txt\"}}}\n", "unknown variable ${id} (${task}, ${vector})"},
		{"wrong kind variable", "x: {kind: scorer, items: {a: {score: \"p/${task}.py\"}}}\n", "unknown variable ${task}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			assert.Error(t, err, tc.want)
		})
	}
}

// structural errors carry the line they came from.
func TestParseErrorLine(t *testing.T) {
	_, err := Parse([]byte("x:\n  kind: task\n  items:\n    a:\n      bogus: 1\n"))
	assert.Error(t, err, "line 5:")
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"unknown harness", base + "b: {kind: agent, items: {b: {harness: bogus, model: m}}}\n", "unknown harness \"bogus\""},
		{"missing harness", base + "b: {kind: agent, items: {b: {model: m}}}\n", "missing harness"},
		{"agent missing model", base + "b: {kind: agent, items: {b: {harness: opencode-openrouter}}}\n", "model is required"},
		{"agent missing sandbox, many defined", base + "s2: {kind: sandbox, items: {s2: {image: img}}}\nb: {kind: agent, items: {b: {harness: claude-cli-keyless, model: m}}}\n", "no sandbox set"},
		{"agent sandbox names a vector of two", base + "boxes: {kind: sandbox, items: {b1: {image: i}, b2: {image: i}}}\nb: {kind: agent, items: {b: {harness: claude-cli-keyless, model: m, sandbox: boxes}}}\n", "names 2 items"},
		{"agent unknown sandbox", base + "b: {kind: agent, items: {b: {harness: claude-cli-keyless, model: m, sandbox: ghost}}}\n", "unknown sandbox \"ghost\""},
		{"effort on an unknown harness", base + "b: {kind: agent, items: {b: {harness: bogus, model: m, effort: high}}}\n", "effort is not supported"},
		{"container needs image", "s: {kind: sandbox, items: {s: {driver: docker}}}\n", "needs an image or a build context"},
		{"image and build", "s: {kind: sandbox, items: {s: {image: img, build: .}}}\n", "mutually exclusive"},
		{"bwrap not implemented", "s: {kind: sandbox, items: {s: {driver: bwrap}}}\n", "bwrap driver not implemented"},
		{"unknown driver", "s: {kind: sandbox, items: {s: {driver: lxc, image: i}}}\n", "unknown driver \"lxc\""},
		{"scorer needs a source", "x: {kind: scorer, items: {x: {}}}\n", "needs a score script or an agent-id"},
		{"scorer with both", "x: {kind: scorer, items: {x: {score: s.py, agent-id: a, prompt: p}}}\n", "mutually exclusive"},
		{"file scorer with prompt", "x: {kind: scorer, items: {x: {score: s.py, prompt: p}}}\n", "prompt is an agent-scorer field"},
		{"agent scorer not implemented", "x: {kind: scorer, items: {x: {agent-id: ghost, prompt: p}}}\n", "agent scorers are not implemented"},
		{"dangling scorer agent-id", "x: {kind: scorer, items: {x: {agent-id: ghost, prompt: p}}}\n", "unknown agent \"ghost\""},
		{"agent scorer without prompt", "x: {kind: scorer, items: {x: {agent-id: ghost}}}\n", "needs a prompt"},
		{"task missing prompt", "x: {kind: task, items: {x: {}}}\n", "prompt is required"},
		{"bad timeout", "x: {kind: task, items: {x: {prompt: p, timeout: soon}}}\n", "bad timeout"},
		{"unknown egress mode", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: firewall}}}}\n", "unknown egress mode"},
		{"deny-urls without mitm-proxy", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, deny-urls: [\"github.com/x*\"]}}}}\n", "deny-urls needs egress mode mitm-proxy"},
		{"deny-urls wildcard host", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: mitm-proxy, deny-urls: [\"*/chisel-releases*\"]}}}}\n", "literal hostname"},
		{"allow-urls without mitm-proxy", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, allow-urls: [\"github.com/x*\"]}}}}\n", "allow-urls needs egress mode mitm-proxy"},
		{"egress allow list under open", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: open, allow: [good.com]}}}}\n", "allow/deny needs egress mode proxy or mitm-proxy"},
		{"egress deny list under none", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: none, deny: [evil.com]}}}}\n", "allow/deny needs egress mode proxy or mitm-proxy"},
		{"egress allow list under default allow", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, default: allow, allow: [good.com]}}}}\n", "allow is ignored under default: allow"},
		{"egress deny list under default deny", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, deny: [evil.com]}}}}\n", "deny is ignored under default: deny"},
		{"duplicate id within a suite axis", base + "su2: {kind: suite, items: {su2: {agents: [a, a], tasks: t}}}\n", "duplicate agent \"a\" in agents"},
		{"duplicate after vector expansion", base + "su2: {kind: suite, items: {su2: {agents: [a, a], tasks: t}}}\n", "duplicate agent"},
		{"unknown axis ref", base + "su2: {kind: suite, items: {su2: {agents: a, tasks: ghost}}}\n", "unknown task \"ghost\" in tasks"},
		{"axis ref of the wrong kind", base + "su2: {kind: suite, items: {su2: {agents: t, tasks: t}}}\n", "unknown agent \"t\""},
		{"suite missing agents", base + "su2: {kind: suite, items: {su2: {tasks: t}}}\n", "agents is required"},
		{"suite missing tasks", base + "su2: {kind: suite, items: {su2: {agents: a}}}\n", "tasks is required"},
		{"duplicate item id across vectors", base + "t2: {kind: task, items: {t: {prompt: p}}}\n", "duplicate task id: t"},
		{"vector id shadows an item of its kind", base + "t2: {kind: task, items: {x: {prompt: p}, y: {prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: t2}}}\nt3: {kind: task, items: {t2: {prompt: p}}}\n", "share an id but name different things"},
		{"slash in id", "x: {kind: task, items: {a/b: {prompt: p}}}\n", "may not contain"},
		{"orphaned task", base + "t2: {kind: task, items: {forgotten: {prompt: p}}}\n", "task \"forgotten\" is in no suite"},
		{"orphaned scorer", base + "sc2: {kind: scorer, items: {lost: {score: s.py}}}\n", "scorer \"lost\" is in no suite"},
		{"orphaned agent", base + "b: {kind: agent, items: {idle: {harness: opencode-openrouter, model: m}}}\n", "agent \"idle\" is in no suite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.Error(t, c.Validate(), tc.want)
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	cases := []struct{ name, src string }{
		{"base", base},
		{"task and scorer share an id (different kinds)", base + "x: {kind: task, items: {same: {prompt: p}}}\nsc2: {kind: scorer, items: {same: {score: s.py}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: same, scorers: same}}}\n"},
		{"singleton vector named after its item", base + "lone: {kind: task, items: {lone: {prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: lone}}}\n"},
		{"sandbox via a vector of one", base + "b: {kind: agent, items: {b: {harness: claude-cli-keyless, model: m, sandbox: s}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"codex with effort", base + "b: {kind: agent, items: {b: {harness: codex-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"copilot with effort", base + "b: {kind: agent, items: {b: {harness: copilot-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"egress proxy allowlist", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, default: deny, allow: [good.com]}}}}\n"},
		{"egress proxy denylist", "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, default: allow, deny: [evil.com]}}}}\n"},
		{"run-only suite", base + "su2: {kind: suite, items: {su2: {agents: a, tasks: t}}}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.NoError(t, c.Validate())
		})
	}
}

// COVER: validateEgress builds the url-rule field/pattern pairs from a slice,
// not a map, so a run with both deny-urls and allow-urls invalid joins its
// errors in a fixed order. pin deny-urls before allow-urls across repeated
// parses.
func TestEgressURLRuleErrorOrderStable(t *testing.T) {
	src := "s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy, deny-urls: [a], allow-urls: [b]}}}}\n"
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

// effortHarnesses is a subset view over Harnesses; pin it so a typo'd or
// removed harness can't linger there.
func TestEffortHarnessesAreHarnesses(t *testing.T) {
	for k := range effortHarnesses {
		assert.That(t, Harnesses[k], "effortHarnesses entry %q is not a harness", k)
	}
}

// FuzzParse just wants Parse to never panic on arbitrary yaml.
func FuzzParse(f *testing.F) {
	seed, err := os.ReadFile("../../pats.example.yaml")
	if err == nil {
		f.Add(seed)
	}
	f.Add([]byte("agents: [foo\n"))
	f.Add([]byte("x: {kind: task, items: {a: {prompt: \"${nope}\"}}}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err == nil {
			_ = c.Validate()
		}
	})
}
