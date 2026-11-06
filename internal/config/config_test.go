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
	c, err := Parse([]byte(src))
	require.NoError(t, err)
	return c
}

// base is a minimal valid config the error cases below extend.
const base = `
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p.txt}}}
sc: {kind: scorer, items: {sc: {score-cmd: sc.py}}}
su: {kind: suite, items: {su: {agents: a, tasks: t, scorers: sc}}}
`

func TestParseVectors(t *testing.T) {
	c := parseT(t, `
slicer:
  kind: task
  defaults:
    prompt-file: prompts/${task}.txt
    prepare-cmd: tasks/${vector}/prepare_${task}.sh
    collect-cmd: tasks/collect.sh
  items:
    base-passwd:
    curl: {timeout: 15m, collect-cmd: ~}
misc:
  kind: task
  items:
    help: {prompt: "write the ${task} readme"}
`)
	require.Len(t, c.Tasks, 3)
	bp, curl, help := c.Tasks[0], c.Tasks[1], c.Tasks[2]

	// file order, vector stamped.
	assert.Equal(t, bp.ID, "base-passwd")
	assert.Equal(t, bp.Vector, "slicer")
	assert.Equal(t, help.Vector, "misc")

	// defaults inherited and interpolated per item.
	assert.Equal(t, bp.PromptFile, "prompts/base-passwd.txt")
	assert.Equal(t, bp.PrepareCmd, "tasks/slicer/prepare_base-passwd.sh")
	assert.Equal(t, bp.CollectCmd, "tasks/collect.sh")
	assert.Equal(t, bp.Timeout, "")
	assert.Equal(t, bp.Dir, "")

	// item overrides a field, and an explicit null unsets one.
	assert.Equal(t, curl.PromptFile, "prompts/curl.txt")
	assert.Equal(t, curl.Timeout, "15m")
	assert.Equal(t, curl.CollectCmd, "")

	// a vector without defaults; an inline prompt is content, not a template.
	assert.Equal(t, help.Prompt, "write the ${task} readme")
}

// a slot (prompt / prompt-file / prompt-cmd, prepare / prepare-cmd, ...)
// merges as a unit: an item setting one key replaces whatever form the
// defaults used, and a null on any key clears the slot.
func TestParseSlotsMergeAsUnits(t *testing.T) {
	c := parseT(t, `
v:
  kind: task
  defaults:
    prompt-file: p.txt
    prepare-cmd: prep.sh
    collect: |
      echo collect
  items:
    inherits:
    inline:
      prompt: literal
      prepare: |
        echo hi ${task}
    cleared:
      collect-cmd: ~
`)
	inherits, inline, cleared := c.Tasks[0], c.Tasks[1], c.Tasks[2]
	assert.Equal(t, inherits.PromptFile, "p.txt")
	assert.Equal(t, inherits.PrepareCmd, "prep.sh")
	assert.Equal(t, inherits.Collect, "echo collect\n")

	assert.Equal(t, inline.Prompt, "literal")
	assert.Equal(t, inline.PromptFile, "")
	assert.Equal(t, inline.Prepare, "echo hi ${task}\n") // script bodies aren't templated
	assert.Equal(t, inline.PrepareCmd, "")

	assert.Equal(t, cleared.Collect, "")
	assert.Equal(t, cleared.CollectCmd, "")
	assert.Equal(t, cleared.PromptFile, "p.txt")
}

// env merges key by key over defaults; a null drops one inherited key. values
// are content, not templates.
func TestParseEnvMergesPerKey(t *testing.T) {
	c := parseT(t, `
v:
  kind: task
  defaults:
    prompt: p
    env:
      MODE: knockout
      KEEP: "1"
  items:
    a:
      env:
        BRANCH: ubuntu-26.04
        KEEP: ~
    b:
      env:
        MODE: denovo
        RAW: ${task}
`)
	a, b := c.Tasks[0], c.Tasks[1]
	assert.Equal(t, len(a.Env), 2)
	assert.Equal(t, a.Env["MODE"], "knockout")
	assert.Equal(t, a.Env["BRANCH"], "ubuntu-26.04")
	assert.Equal(t, b.Env["MODE"], "denovo")
	assert.Equal(t, b.Env["KEEP"], "1")
	assert.Equal(t, b.Env["RAW"], "${task}")
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
  defaults: &py {score-cmd: "scorers/${scorer}.py"}
  items: {one: ~, two: ~}
transcript:
  kind: scorer
  defaults: *py
  items: {three: ~}
misc:
  kind: task
  defaults: &td {prompt-file: p.txt, collect-cmd: c.sh}
  items: {x: ~}
misc2:
  kind: task
  defaults: {<<: *td, collect-cmd: ~, timeout: 5m}
  items: {y: ~, z: {collect-cmd: z.sh}}
`)
	assert.Equal(t, c.Scorers[2].ScoreCmd, "scorers/three.py")
	assert.Equal(t, c.Scorers[2].Vector, "transcript")
	y, z := c.Tasks[1], c.Tasks[2]
	assert.Equal(t, y.PromptFile, "p.txt")
	assert.Equal(t, y.CollectCmd, "")
	assert.Equal(t, y.Timeout, "5m")
	assert.Equal(t, z.CollectCmd, "z.sh")
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
		{"missing items", "x: {kind: task, defaults: {prompt: p}}\n", "items or dir is required"},
		{"empty items", "x: {kind: task, items: {}}\n", "items is empty"},
		{"items not a mapping", "x: {kind: task, items: [a, b]}\n", "items: must be a mapping"},
		{"item not a mapping", "x: {kind: task, items: {a: [1]}}\n", "task \"a\": must be a mapping"},
		{"unknown field on item", "x: {kind: task, items: {a: {bogus: 1}}}\n", "task \"a\": unknown field \"bogus\""},
		{"unknown field in defaults", "x: {kind: agent, defaults: {kind: claude-cli-keyless}, items: {a: ~}}\n", "defaults: unknown field \"kind\""},
		{"unknown template variable", "x: {kind: task, items: {a: {prompt-file: \"p/${id}.txt\"}}}\n", "unknown variable ${id} (${task}, ${vector})"},
		{"wrong kind variable", "x: {kind: scorer, items: {a: {score-cmd: \"p/${task}.py\"}}}\n", "unknown variable ${task}"},
		{"two keys of one slot", "x: {kind: task, items: {a: {prompt: p, prompt-file: f}}}\n", "prompt, prompt-file are mutually exclusive"},
		{"two keys of one slot in defaults", "x: {kind: task, defaults: {prepare: a, prepare-cmd: b}, items: {a: ~}}\n", "prepare, prepare-cmd are mutually exclusive"},
		{"agent-id is renamed agent", "x: {kind: scorer, items: {a: {agent-id: j}}}\n", "unknown field \"agent-id\""},
		{"dir on a scorer vector", "x: {kind: scorer, dir: scorers}\n", "dir is only supported on task vectors"},
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
		{"image and build", "s: {kind: sandbox, items: {s: {image: img, build: .}}}\n", "mutually exclusive"},
		{"bwrap not implemented", "s: {kind: sandbox, items: {s: {driver: bwrap}}}\n", "bwrap driver not implemented"},
		{"unknown driver", "s: {kind: sandbox, items: {s: {driver: lxc, image: i}}}\n", "unknown driver \"lxc\""},
		{"scorer needs a source", "x: {kind: scorer, items: {x: {}}}\n", "needs score, score-cmd or agent"},
		{"scorer with both", "x: {kind: scorer, items: {x: {score-cmd: s.py, agent: a, prompt: p}}}\n", "mutually exclusive"},
		{"script scorer with prompt", "x: {kind: scorer, items: {x: {score-cmd: s.py, prompt: p}}}\n", "a prompt is an agent-scorer field"},
		{"agent scorer not implemented", "x: {kind: scorer, items: {x: {agent: ghost, prompt: p}}}\n", "agent scorers are not implemented"},
		{"dangling scorer agent", "x: {kind: scorer, items: {x: {agent: ghost, prompt: p}}}\n", "unknown agent \"ghost\""},
		{"agent scorer without prompt", "x: {kind: scorer, items: {x: {agent: ghost}}}\n", "needs one of prompt, prompt-file or prompt-cmd"},
		{"task missing prompt", "x: {kind: task, items: {x: {}}}\n", "needs one of prompt, prompt-file or prompt-cmd"},
		{"bad env name", "x: {kind: task, items: {x: {prompt: p, env: {1BAD: v}}}}\n", "not a valid variable name"},
		{"env in the PATS_ namespace", "x: {kind: task, items: {x: {prompt: p, env: {PATS_TASK_ID: v}}}}\n", "PATS_* is reserved"},
		{"env PATH", "x: {kind: task, items: {x: {prompt: p, env: {PATH: /x}}}}\n", "PATH is set by pats"},
		{"bad timeout", "x: {kind: task, items: {x: {prompt: p, timeout: soon}}}\n", "bad timeout"},
		{"egress wildcard host", "s: {kind: sandbox, items: {s: {image: img, egress: {deny: [\"*/chisel-releases*\"]}}}}\n", "literal hostname"},
		{"egress bad default", "s: {kind: sandbox, items: {s: {image: img, egress: {default: maybe}}}}\n", "want deny or allow"},
		{"egress bad deny-response", "s: {kind: sandbox, items: {s: {image: img, egress: {deny-response: 500}}}}\n", "want 403, 404, reset or timeout"},
		{"egress bad regexp", "s: {kind: sandbox, items: {s: {image: img, egress: {allow: ['a.test ~(']}}}}\n", "regexp"},
		{"egress methods not implemented", "s: {kind: sandbox, items: {s: {image: img, egress: {methods: [GET]}}}}\n", "methods is not implemented yet"},
		{"egress method prefix not implemented", "s: {kind: sandbox, items: {s: {image: img, egress: {allow: ['GET a.test/*']}}}}\n", "method prefix is not implemented yet"},
		{"egress dead allow under default allow", "s: {kind: sandbox, items: {s: {image: img, egress: {default: allow, allow: [good.com]}}}}\n", "is dead under default: allow"},
		{"egress image under bwrap", "s: {kind: sandbox, items: {s: {driver: bwrap, egress: {image: p}}}}\n", "needs a container driver"},
		{"sandbox env bad name", "s: {kind: sandbox, items: {s: {image: img, env: {1X: v}}}}\n", "not a valid variable name"},
		{"sandbox env PATS_", "s: {kind: sandbox, items: {s: {image: img, env: {PATS_X: v}}}}\n", "PATS_* is reserved"},
		{"sandbox env HOME", "s: {kind: sandbox, items: {s: {image: img, env: {HOME: /x}}}}\n", "env HOME is set by pats"},
		{"task egress bad entry", "s: {kind: sandbox, items: {s: {image: img, egress: {}}}}\na: {kind: agent, items: {a: {harness: opencode-openrouter, model: m}}}\nt: {kind: task, items: {t: {prompt: p, egress: {deny: ['*/x']}}}}\nsu: {kind: suite, items: {su: {agents: a, tasks: t}}}\n", "literal hostname"},
		{"task egress under an open sandbox", base + "x: {kind: task, items: {x: {prompt: p, egress: {deny: ['a.test/x*']}}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: x}}}\n", "whose egress is open"},
		{"task egress dead under a denylist", "s: {kind: sandbox, items: {s: {image: img, egress: {default: allow}}}}\na: {kind: agent, items: {a: {harness: opencode-openrouter, model: m}}}\nt: {kind: task, items: {t: {prompt: p, egress: {allow: [pypi.org]}}}}\nsu: {kind: suite, items: {su: {agents: a, tasks: t}}}\n", "is dead under sandbox"},
		{"egress none under an agent", "s: {kind: sandbox, items: {s: {image: img, egress: none}}}\na: {kind: agent, items: {a: {harness: opencode-openrouter, model: m}}}\nt: {kind: task, items: {t: {prompt: p}}}\nsu: {kind: suite, items: {su: {agents: a, tasks: t}}}\n", "needs the network"},
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
		{"orphaned scorer", base + "sc2: {kind: scorer, items: {lost: {score-cmd: s.py}}}\n", "scorer \"lost\" is in no suite"},
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
		{"task and scorer share an id (different kinds)", base + "x: {kind: task, items: {same: {prompt: p}}}\nsc2: {kind: scorer, items: {same: {score: \"echo 1\"}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: same, scorers: same}}}\n"},
		{"singleton vector named after its item", base + "lone: {kind: task, items: {lone: {prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: a, tasks: lone}}}\n"},
		{"sandbox via a vector of one", base + "b: {kind: agent, items: {b: {harness: claude-cli-keyless, model: m, sandbox: s}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"codex with effort", base + "b: {kind: agent, items: {b: {harness: codex-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"copilot with effort", base + "b: {kind: agent, items: {b: {harness: copilot-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: b, tasks: t}}}\n"},
		{"sandbox with neither image nor build (the published default)", "s: {kind: sandbox, items: {s: {driver: docker}}}\n"},
		{"egress allowlist", "s: {kind: sandbox, items: {s: {image: img, egress: {allow: [good.com, 'github.com:22', '10.0.0.5:5432']}}}}\n"},
		{"egress denylist", "s: {kind: sandbox, items: {s: {image: img, egress: {default: allow, deny: [evil.com]}}}}\n"},
		{"egress url allow under default allow", "s: {kind: sandbox, items: {s: {image: img, egress: {default: allow, allow: ['a.test/public*']}}}}\n"},
		{"egress url entries and a regexp", "s: {kind: sandbox, items: {s: {image: img, egress: {allow: ['.githubusercontent.com/canonical/*', 'api.github.com ~^/repos/'], deny: ['github.com/*/x*']}}}}\n"},
		{"task egress under a proxy sandbox", "s: {kind: sandbox, items: {s: {image: img, egress: {allow: [a.test]}}}}\na: {kind: agent, items: {a: {harness: opencode-openrouter, model: m}}}\nt: {kind: task, items: {t: {prompt: p, egress: {allow: [pypi.org], deny: ['github.com/*/answer*']}}}}\nsu: {kind: suite, items: {su: {agents: a, tasks: t}}}\n"},
		{"egress image under podman", "s: {kind: sandbox, items: {s: {driver: podman, image: img, egress: {image: p}}}}\n"},
		{"sandbox env", "s: {kind: sandbox, items: {s: {image: img, env: {DENO_CERT: /sbx-ca/bundle.pem}}}}\n"},
		{"egress none with no agents", "s: {kind: sandbox, items: {s: {image: img, egress: none}}}\n"},
		{"egress empty mapping is a proxy", "s: {kind: sandbox, items: {s: {image: img, egress: {}}}}\n"},
		{"run-only suite", base + "su2: {kind: suite, items: {su2: {agents: a, tasks: t}}}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.NoError(t, c.Validate())
		})
	}
}

// an egress mapping's keys are checked by its own unmarshal (the loader's
// unknown-field check stops at the item), and the only scalar is none.
func TestEgressParseErrors(t *testing.T) {
	_, err := Parse([]byte("s: {kind: sandbox, items: {s: {image: img, egress: {mode: proxy}}}}\n"))
	assert.Error(t, err, "egress: unknown field \"mode\"")
	_, err = Parse([]byte("s: {kind: sandbox, items: {s: {image: img, egress: open}}}\n"))
	assert.Error(t, err, "want none or a mapping")
	// a task carries no posture: that is the sandbox's.
	_, err = Parse([]byte("x: {kind: task, items: {x: {prompt: p, egress: {default: allow}}}}\n"))
	assert.Error(t, err, "unknown field \"default\"")
	_, err = Parse([]byte("x: {kind: task, items: {x: {prompt: p, egress: none}}}\n"))
	assert.Error(t, err, "a task takes a mapping")
}

// the three shapes of `egress`: absent is open, the scalar none is no
// network, a mapping (even empty) is the proxy.
func TestEgressShapes(t *testing.T) {
	c := parseT(t, "s: {kind: sandbox, items: {open: {image: i}, off: {image: i, egress: none}, p: {image: i, egress: {allow: [a.test]}}, nulled: {image: i, egress: ~}}}\n")
	modes := map[string]string{}
	for _, sb := range c.Sandboxes {
		modes[sb.ID] = sb.Egress.Mode()
	}
	assert.Equal(t, modes["open"], "open")
	assert.Equal(t, modes["off"], "none")
	assert.Equal(t, modes["p"], "proxy")
	assert.Equal(t, modes["nulled"], "open")
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
	f.Add([]byte("x: {kind: task, items: {a: {prompt-file: \"${nope}\"}}}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err == nil {
			_ = c.Validate()
		}
	})
}

// task egress follows the vectors rule: an item's block replaces the
// defaults' whole, and a null drops it.
func TestTaskEgressDefaults(t *testing.T) {
	c := parseT(t, `x:
  kind: task
  defaults: {prompt: p, egress: {deny: ['a.test/x*'], allow: [b.test]}}
  items:
    inherits: ~
    replaces: {egress: {allow: [c.test]}}
    drops: {egress: ~}
`)
	got := map[string]TaskEgress{}
	for _, tk := range c.Tasks {
		got[tk.ID] = tk.Egress
	}
	assert.EqualArrays(t, got["inherits"].Deny, []string{"a.test/x*"})
	assert.EqualArrays(t, got["replaces"].Allow, []string{"c.test"})
	assert.Equal(t, len(got["replaces"].Deny), 0)
	assert.Equal(t, got["drops"].Set, false)
}
