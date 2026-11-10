package config

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox/proxy"
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
	// 4 scorers (one an agent scorer) x 3 tasks = 12 pairs.
	assert.Len(t, score, 12)
}

func parseT(t *testing.T, src string) *Config {
	t.Helper()
	c, err := Parse([]byte(src))
	require.NoError(t, err)
	return c
}

// base is a minimal valid config the error cases below extend.
const base = `
sxx: {kind: sandbox, items: {sxx: {image: img}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p.txt}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx, scorers: scx}}}
`

func TestParseVectors(t *testing.T) {
	c := parseT(t, `
slicer:
  kind: task
  defaults:
    prompt-file: prompt.txt
    stage: tasks/prepare.sh
    collect: tasks/collect.sh
  items:
    base-passwd:
    curl: {timeout: 15m, collect: ~}
misc:
  kind: task
  items:
    help: {prompt: "write the ${task} readme"}
`)
	require.Len(t, c.Tasks, 3)
	bp, curl, help := c.Tasks[0], c.Tasks[1], c.Tasks[2]

	// file order, vector stamped.
	assert.Equal(t, bp.Name, "base-passwd")
	assert.Equal(t, bp.Vector, "slicer")
	assert.Equal(t, help.Vector, "misc")

	// defaults inherited by each item, as written.
	assert.Equal(t, bp.PromptFile, "prompt.txt")
	assert.Equal(t, bp.Stage, "tasks/prepare.sh")
	assert.Equal(t, bp.Collect, "tasks/collect.sh")
	assert.Equal(t, bp.Timeout, "")
	assert.Equal(t, bp.Dir, "")

	// item overrides a field, and an explicit null unsets one.
	assert.Equal(t, curl.PromptFile, "prompt.txt")
	assert.Equal(t, curl.Timeout, "15m")
	assert.Equal(t, curl.Collect, "")

	// a vector without defaults; a prompt's text is the agent's, as written.
	assert.Equal(t, help.Prompt, "write the ${task} readme")
}

// the prompt's slot (prompt / prompt-file / prompt-cmd) merges as a unit: an
// item setting one key replaces whatever form the defaults used, and a null
// on any key clears it. a step is one key, merged like any other.
func TestParseSlotsMergeAsUnits(t *testing.T) {
	c := parseT(t, `
vxx:
  kind: task
  defaults:
    prompt-file: p.txt
    stage: ./prep.sh
    collect: |
      echo collect
  items:
    inherits:
    inline:
      prompt: literal
      stage: |
        echo hi "$PATS_TASK_NAME"
    cleared:
      collect: ~
      prompt-cmd: ~
`)
	inherits, inline, cleared := c.Tasks[0], c.Tasks[1], c.Tasks[2]
	assert.Equal(t, inherits.PromptFile, "p.txt")
	assert.Equal(t, inherits.Stage, "./prep.sh")
	assert.Equal(t, inherits.Collect, "echo collect\n")

	assert.Equal(t, inline.Prompt, "literal")
	assert.Equal(t, inline.PromptFile, "")
	assert.Equal(t, inline.Stage, "echo hi \"$PATS_TASK_NAME\"\n")

	assert.Equal(t, cleared.Collect, "")
	assert.Equal(t, cleared.Stage, "./prep.sh")
	assert.Equal(t, cleared.PromptFile, "")
}

// env merges key by key over defaults; a null drops one inherited key. values
// are content, not templates.
func TestParseEnvMergesPerKey(t *testing.T) {
	c := parseT(t, `
vxx:
  kind: task
  defaults:
    prompt: p
    env:
      MODE: knockout
      KEEP: "1"
  items:
    axx:
      env:
        BRANCH: ubuntu-26.04
        KEEP: ~
    bxx:
      env:
        MODE: denovo
        RAW: $PATS_TASK_NAME
`)
	a, b := c.Tasks[0], c.Tasks[1]
	assert.Equal(t, len(a.Env), 2)
	assert.Equal(t, a.Env["MODE"], "knockout")
	assert.Equal(t, a.Env["BRANCH"], "ubuntu-26.04")
	assert.Equal(t, b.Env["MODE"], "denovo")
	assert.Equal(t, b.Env["KEEP"], "1")
	assert.Equal(t, b.Env["RAW"], "$PATS_TASK_NAME")
}

func TestParseSandboxDriverDefault(t *testing.T) {
	c := parseT(t, `
sxx:
  kind: sandbox
  items:
    dxx: {image: img}
    pxx: {driver: podman, image: img}
`)
	assert.Equal(t, c.Sandboxes[0].Driver, "docker")
	assert.Equal(t, c.Sandboxes[1].Driver, "podman")
}

// anchors + merge keys are how defaults are shared between vectors.
func TestParseDefaultsViaAnchors(t *testing.T) {
	c := parseT(t, `
sdf:
  kind: scorer
  defaults: &py {score: "scorers/scorer_$PATS_SCORER_NAME.py"}
  items: {one: ~, two: ~}
transcript:
  kind: scorer
  defaults: *py
  items: {three: ~}
misc:
  kind: task
  defaults: &td {prompt-file: p.txt, collect: ./c.sh}
  items: {xxx: ~}
misc2:
  kind: task
  defaults: {<<: *td, collect: ~, timeout: 5m}
  items: {yxx: ~, zxx: {collect: ./z.sh}}
`)
	assert.Equal(t, c.Scorers[2].Score, "scorers/scorer_$PATS_SCORER_NAME.py")
	assert.Equal(t, c.Scorers[2].Vector, "transcript")
	y, z := c.Tasks[1], c.Tasks[2]
	assert.Equal(t, y.PromptFile, "p.txt")
	assert.Equal(t, y.Collect, "")
	assert.Equal(t, y.Timeout, "5m")
	assert.Equal(t, z.Collect, "./z.sh")
}

func TestParseSuiteDefaults(t *testing.T) {
	c := parseT(t, `
suites:
  kind: suite
  defaults: {agents: all, scorers: [xxx, yxx]}
  items:
    one: {tasks: t1x}
    two: {tasks: [t2x, t3x], scorers: zxx}
`)
	assert.EqualArrays(t, []string(c.Suites[0].Agents), []string{"all"})
	assert.EqualArrays(t, []string(c.Suites[0].Tasks), []string{"t1x"})
	assert.EqualArrays(t, []string(c.Suites[0].Scorers), []string{"xxx", "yxx"})
	assert.EqualArrays(t, []string(c.Suites[1].Tasks), []string{"t2x", "t3x"})
	assert.EqualArrays(t, []string(c.Suites[1].Scorers), []string{"zxx"})
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"top level not a mapping", "- a\n", "top level must be a mapping"},
		{"vector not a mapping", "xxx: [1]\n", "vector xxx: must be a mapping"},
		{"missing kind", "xxx: {items: {axx: ~}}\n", "vector \"xxx\": missing kind"},
		{"unknown kind", "xxx: {kind: tasks, items: {axx: ~}}\n", "unknown kind \"tasks\""},
		{"unknown vector key", "xxx: {kind: task, item: {axx:}}\n", "unknown key \"item\""},
		{"missing items", "xxx: {kind: task, defaults: {prompt: p}}\n", "items or dir is required"},
		{"empty items", "xxx: {kind: task, items: {}}\n", "items is empty"},
		{"items not a mapping", "xxx: {kind: task, items: [axx, bxx]}\n", "items: must be a mapping"},
		{"item not a mapping", "xxx: {kind: task, items: {axx: [1]}}\n", "task \"axx\": must be a mapping"},
		{"unknown field on item", "xxx: {kind: task, items: {axx: {bogus: 1}}}\n", "task \"axx\": unknown field \"bogus\""},
		{"unknown field in defaults", "xxx: {kind: agent, defaults: {kind: claude-cli-keyless}, items: {axx: ~}}\n", "defaults: unknown field \"kind\""},
		{"a template in a path", "xxx: {kind: task, items: {axx: {prompt-file: \"p/${task}.txt\"}}}\n", "${task}: pats.yaml is not templated; a step reads the name from $PATS_TASK_NAME"},
		{"a template in a script", "xxx: {kind: scorer, items: {axx: {score: \"p/${scorer}.py\"}}}\n", "${scorer}: pats.yaml is not templated; a step reads the name from $PATS_SCORER_NAME"},
		{"a vector template in a task", "xxx: {kind: task, defaults: {prompt-cmd: \"cat ${vector}.txt\"}, items: {axx: ~}}\n", "a step reads the name from $PATS_TASK_VECTOR"},
		{"a template in env", "xxx: {kind: task, items: {axx: {prompt: p, env: {X: \"${agent}\"}}}}\n", "${agent}: pats.yaml is not templated"},
		{"a template in egress", "xxx: {kind: task, items: {axx: {prompt: p, egress: {allow: [\"${task}.example.com\"]}}}}\n", "${task}: pats.yaml is not templated"},
		{"a template on a sandbox", "xxx: {kind: sandbox, items: {sxx: {image: \"img-${sandbox}\"}}}\n", "${sandbox}: pats.yaml is not templated, and no step env var carries it"},
		{"a vector template on an agent", "xxx: {kind: agent, items: {axx: {sandbox: \"${vector}\"}}}\n", "${vector}: pats.yaml is not templated, and no step env var carries it"},
		{"a template in defaults every item overrides", "xxx: {kind: task, defaults: {prompt-file: \"p/${task}.txt\"}, items: {axx: {prompt: hello}}}\n", "defaults: ${task}: pats.yaml is not templated"},
		{"a template in a dir", "xxx: {kind: task, dir: \"cases/${vector}\"}\n", "dir: ${vector}: pats.yaml is not templated"},
		{"stage-cmd is gone", "xxx: {kind: task, items: {axx: {prompt: p, stage-cmd: ./s.sh}}}\n", "stage-cmd is gone: use stage, a shell script"},
		{"score-cmd is gone, in defaults", "xxx: {kind: scorer, defaults: {score-cmd: ./s.py}, items: {axx: ~}}\n", "score-cmd is gone: use score"},
		{"provision-cmd is gone", "xxx: {kind: sandbox, items: {sxx: {provision-cmd: ./p.sh}}}\n", "provision-cmd is gone: use provision"},
		{"two keys of one slot", "xxx: {kind: task, items: {axx: {prompt: p, prompt-file: f}}}\n", "prompt, prompt-file are mutually exclusive"},
		{"two keys of one slot in defaults", "xxx: {kind: task, defaults: {prompt: a, prompt-cmd: b}, items: {axx: ~}}\n", "prompt, prompt-cmd are mutually exclusive"},
		{"agent-id is renamed agent", "xxx: {kind: scorer, items: {axx: {agent-id: j}}}\n", "unknown field \"agent-id\""},
		{"dir on a scorer vector", "xxx: {kind: scorer, dir: scorers}\n", "dir is only supported on task vectors"},
		{"a mapping as a key, beside a merge", "xxx: {<<, {}}\n", "a key must be a plain value"},
		{"a list as a key, nested", "xxx: {kind: task, items: {axx: {prompt: p, env: {[a]: b}}}}\n", "a key must be a plain value"},
		{"an aliased mapping as a key", "xxx: {kind: task, defaults: &d {prompt: p}, items: {*d : ~}}\n", "a key must be a plain value"},
		{"a null item key", "xxx: {kind: task, defaults: {env: 1}, items: {~: ~}}\n", "an item is keyed by its name, not by a null key"},
		{"a binary item key", "xxx: {kind: task, defaults: {prompt: q}, items: {!!binary abcd: {prompt: p}}}\n", "an item is keyed by its name, not by a binary key"},
		{"a merge among items", "xxx: {kind: task, defaults: &d {prompt: p}, items: {axx: ~, <<: *d}}\n", "an item is keyed by its name, not by a merge key"},
		{"an aliased null as an item key", "xxx: {kind: task, defaults: {prompt: &n ~}, items: {*n : ~}}\n", "an item is keyed by its name, not by a null key"},
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
	_, err := Parse([]byte("xxx:\n  kind: task\n  items:\n    axx:\n      bogus: 1\n"))
	assert.Error(t, err, "line 5:")
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"unknown harness", base + "bxx: {kind: agent, items: {bxx: {harness: bogus, model: m}}}\n", "unknown harness \"bogus\""},
		{"missing harness", base + "bxx: {kind: agent, items: {bxx: {model: m}}}\n", "missing harness"},
		{"agent missing model", base + "bxx: {kind: agent, items: {bxx: {harness: opencode-openrouter}}}\n", "model is required"},
		{"agent missing sandbox, many defined", base + "s2x: {kind: sandbox, items: {s2x: {image: img}}}\nbxx: {kind: agent, items: {bxx: {harness: claude-cli-keyless, model: m}}}\n", "no sandbox set"},
		{"agent sandbox names a vector of two", base + "boxes: {kind: sandbox, items: {b1x: {image: i}, b2x: {image: i}}}\nbxx: {kind: agent, items: {bxx: {harness: claude-cli-keyless, model: m, sandbox: boxes}}}\n", "names 2 items"},
		{"agent unknown sandbox", base + "bxx: {kind: agent, items: {bxx: {harness: claude-cli-keyless, model: m, sandbox: ghost}}}\n", "unknown sandbox \"ghost\""},
		{"effort on an unknown harness", base + "bxx: {kind: agent, items: {bxx: {harness: bogus, model: m, effort: high}}}\n", "effort is not supported"},
		{"max-cost on a harness without live cost", base + "bxx: {kind: agent, items: {bxx: {harness: claude-cli-keyless, model: m, max-cost: 2}}}\n", "max-cost needs a harness that reports cost as it runs"},
		{"env a shell will not set", base + "s2x: {kind: sandbox, items: {s2x: {image: img, env: {UID: '1'}}}}\n", "read-only in a shell"},
		{"max-cost zero", base + "bxx: {kind: agent, items: {bxx: {harness: opencode-openrouter, model: m, max-cost: 0}}}\n", "is not a cost cap"},
		{"max-cost negative", base + "bxx: {kind: agent, items: {bxx: {harness: opencode-openrouter, model: m, max-cost: -1}}}\n", "is not a cost cap"},
		{"image and build", "sxx: {kind: sandbox, items: {sxx: {image: img, build: .}}}\n", "mutually exclusive"},
		{"bwrap build", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, build: .}}}\n", "build is refused under the bwrap driver"},
		{"bwrap provision", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, provision: p}}}\n", "provision is refused under the bwrap driver"},
		{"bwrap limits", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, limits: {memory: 1GiB}}}}\n", "limits is refused under the bwrap driver"},
		{"bwrap sudo", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, sudo: true}}}\n", "sudo is refused under the bwrap driver"},
		{"bwrap egress image", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, egress: {allow: [a.test], image: i}}}}\n", "egress image needs a container driver"},
		{"unknown driver", "sxx: {kind: sandbox, items: {sxx: {driver: lxc, image: i}}}\n", "unknown driver \"lxc\""},
		{"host without the key", "sxx: {kind: sandbox, items: {sxx: {driver: host}}}\n", "the host driver runs the agent on this machine, as you, with no isolation; set sand-not-boxed: true"},
		{"the key elsewhere", "sxx: {kind: sandbox, items: {sxx: {image: i, sand-not-boxed: true}}}\n", "sand-not-boxed is the host driver's (driver \"docker\")"},
		{"host image", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, image: i}}}\n", "image is refused under the host driver"},
		{"host build", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, build: .}}}\n", "build is refused under the host driver"},
		{"host base", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, base: b}}}\n", "base is refused under the host driver"},
		{"host provision", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, provision: p}}}\n", "provision is refused under the host driver"},
		{"host limits", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, limits: {cpu: 2}}}}\n", "limits is refused under the host driver"},
		{"host sudo", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, sudo: true}}}\n", "sudo is refused under the host driver"},
		{"host vm", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, vm: true}}}\n", "vm is refused under the host driver"},
		{"host remote", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, remote: r}}}\n", "remote is refused under the host driver"},
		{"host profiles", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, profiles: [p]}}}\n", "profiles is refused under the host driver"},
		{"host config", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, config: {a: b}}}}\n", "config is refused under the host driver"},
		{"host egress none", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, egress: none}}}\n", "egress: none is refused under the host driver"},
		{"host egress image", "sxx: {kind: sandbox, items: {sxx: {driver: host, sand-not-boxed: true, egress: {allow: [a.test], image: i}}}}\n", "egress image needs a container driver"},
		{"scorer needs a source", "xxx: {kind: scorer, items: {xxx: {}}}\n", "needs score or agent"},
		{"scorer with both", "xxx: {kind: scorer, items: {xxx: {score: ./s.py, agent: axx, prompt: p}}}\n", "mutually exclusive"},
		{"script scorer with prompt", "xxx: {kind: scorer, items: {xxx: {score: ./s.py, prompt: p}}}\n", "a prompt is an agent-scorer field"},
		{"script scorer with a judge field", "xxx: {kind: scorer, items: {xxx: {score: ./s.py, stage: echo}}}\n", "stage is an agent-scorer field"},
		{"judge vector", base + "jvx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m}}}\nsc2: {kind: scorer, items: {sc2: {agent: jvx, prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "agent \"jvx\" is a vector"},
		{"judge env", base + "jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}\n" + "sc2: {kind: scorer, items: {sc2: {agent: jxx, prompt: p, env: {PATS_X: y}}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "scorer \"sc2\": env \"PATS_X\": PATS_* is reserved"},
		{"judge timeout", base + "jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}\n" + "sc2: {kind: scorer, items: {sc2: {agent: jxx, prompt: p, timeout: soon}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "bad timeout \"soon\""},
		{"judge egress, open sandbox", base + "jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}\n" + "sc2: {kind: scorer, items: {sc2: {agent: jxx, prompt: p, egress: {allow: [x.com]}}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "scorer \"sc2\" sets egress, but its agent \"jxx\""},
		{"judge sudo under docker", base + "jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}\n" + "sc2: {kind: scorer, items: {sc2: {agent: jxx, prompt: p, sudo: true}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "sets sudo"},
		{"dangling scorer agent", "xxx: {kind: scorer, items: {xxx: {agent: ghost, prompt: p}}}\n", "unknown agent \"ghost\""},
		{"agent scorer without prompt", "xxx: {kind: scorer, items: {xxx: {agent: ghost}}}\n", "needs one of prompt, prompt-file or prompt-cmd"},
		{"task missing prompt", "xxx: {kind: task, items: {xxx: {}}}\n", "needs one of prompt, prompt-file or prompt-cmd"},
		{"bad env name", "xxx: {kind: task, items: {xxx: {prompt: p, env: {1BAD: v}}}}\n", "not a valid variable name"},
		{"env in the PATS_ namespace", "xxx: {kind: task, items: {xxx: {prompt: p, env: {PATS_TASK_NAME: v}}}}\n", "PATS_* is reserved"},
		{"env PATH", "xxx: {kind: task, items: {xxx: {prompt: p, env: {PATH: /x}}}}\n", "PATH is set by pats"},
		{"bad timeout", "xxx: {kind: task, items: {xxx: {prompt: p, timeout: soon}}}\n", "bad timeout"},
		{"negative timeout", "xxx: {kind: task, items: {xxx: {prompt: p, timeout: -5m}}}\n", "bad timeout \"-5m\""},
		{"negative judge timeout", base + "jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}\n" + "sc2: {kind: scorer, items: {sc2: {agent: jxx, prompt: p, timeout: -1s}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: sc2}}}\n", "bad timeout \"-1s\""},
		{"egress wildcard host", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {deny: [\"*/chisel-releases*\"]}}}}\n", "literal hostname"},
		{"egress bad default", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {default: maybe}}}}\n", "want deny or allow"},
		{"egress bad deny-response", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {deny-response: 500}}}}\n", "want 403, 404, reset or timeout"},
		{"egress bad regexp", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {allow: ['a.test ~(']}}}}\n", "regexp"},
		{"egress empty methods", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {methods: []}}}}\n", "an empty list would refuse every request"},
		{"egress bad method", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {methods: [get]}}}}\n", "not an upper-case http method"},
		{"egress harness set by hand", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {harness: [x.test]}}}}\n", "harness is set by pats"},
		{"egress method on a bare host", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {allow: ['GET a.test']}}}}\n", "a method needs a url entry"},
		{"egress dead allow under default allow", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {default: allow, allow: [good.com]}}}}\n", "is dead under default: allow"},
		{"egress image under bwrap", "sxx: {kind: sandbox, items: {sxx: {driver: bwrap, egress: {image: p}}}}\n", "needs a container driver"},
		{"sandbox env bad name", "sxx: {kind: sandbox, items: {sxx: {image: img, env: {1X: v}}}}\n", "not a valid variable name"},
		{"sandbox env PATS_", "sxx: {kind: sandbox, items: {sxx: {image: img, env: {PATS_X: v}}}}\n", "PATS_* is reserved"},
		{"sandbox env HOME", "sxx: {kind: sandbox, items: {sxx: {image: img, env: {HOME: /x}}}}\n", "env HOME is set by pats"},
		{"task egress bad entry", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {}}}}\naxx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}}}\ntxx: {kind: task, items: {txx: {prompt: p, egress: {deny: ['*/x']}}}}\nsux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}\n", "literal hostname"},
		{"task egress under an open sandbox", base + "xxx: {kind: task, items: {xxx: {prompt: p, egress: {deny: ['a.test/x*']}}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: xxx}}}\n", "whose egress is open"},
		{"task egress dead under a denylist", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {default: allow}}}}\naxx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}}}\ntxx: {kind: task, items: {txx: {prompt: p, egress: {allow: [pypi.org]}}}}\nsux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}\n", "is dead under sandbox"},
		{"egress none under an agent", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: none}}}\naxx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}}}\ntxx: {kind: task, items: {txx: {prompt: p}}}\nsux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}\n", "needs the network"},
		{"duplicate id within a suite axis", base + "su2: {kind: suite, items: {su2: {agents: [axx, axx], tasks: txx}}}\n", "duplicate agent \"axx\" in agents"},
		{"duplicate after vector expansion", base + "su2: {kind: suite, items: {su2: {agents: [axx, axx], tasks: txx}}}\n", "duplicate agent"},
		{"unknown axis ref", base + "su2: {kind: suite, items: {su2: {agents: axx, tasks: ghost}}}\n", "unknown task \"ghost\" in tasks"},
		{"axis ref of the wrong kind", base + "su2: {kind: suite, items: {su2: {agents: txx, tasks: txx}}}\n", "unknown agent \"txx\""},
		{"suite missing agents", base + "su2: {kind: suite, items: {su2: {tasks: txx}}}\n", "agents is required"},
		{"suite missing tasks", base + "su2: {kind: suite, items: {su2: {agents: axx}}}\n", "tasks is required"},
		{"duplicate item name across vectors", base + "t2x: {kind: task, items: {txx: {prompt: p}}}\n", "duplicate task name: txx"},
		{"vector name shadows an item of its kind", base + "t2x: {kind: task, items: {xxx: {prompt: p}, yxx: {prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: t2x}}}\nt3x: {kind: task, items: {t2x: {prompt: p}}}\n", "share a name but are different things"},
		{"agent named as a run file", "sxx: {kind: sandbox, items: {sxx: {image: img}}}\naxx: {kind: agent, items: {scores.json: {harness: opencode-openrouter, model: m}}}\n", "the name is taken by a run's own scores.json"},
		{"slash in name", "xxx: {kind: task, items: {a/b: {prompt: p}}}\n", "task \"a/b\" is not a name: lowercase letters, digits, dots, pluses and single hyphens, a letter or digit first, no hyphen last, at least 2 long"},
		{"orphaned task", base + "t2x: {kind: task, items: {forgotten: {prompt: p}}}\n", "task \"forgotten\" is in no suite"},
		{"orphaned scorer", base + "sc2: {kind: scorer, items: {lost: {score: ./s.py}}}\n", "scorer \"lost\" is in no suite"},
		{"orphaned agent", base + "bxx: {kind: agent, items: {idle: {harness: opencode-openrouter, model: m}}}\n", "agent \"idle\" is in no suite"},
		{"lxd key under docker", base + "dxx: {kind: sandbox, items: {dxx: {vm: true}}}\n", "vm is lxd-only"},
		{"sudo under docker", base + "dxx: {kind: sandbox, items: {dxx: {sudo: true}}}\n", "sudo is lxd-only"},
		{"image and base", base + "dxx: {kind: sandbox, items: {dxx: {image: i, base: ubuntu:26.04}}}\n", "image and base / provision are mutually exclusive"},
		{"build under lxd", base + "lxx: {kind: sandbox, items: {lxx: {driver: lxd, build: .}}}\nlax: {kind: agent, items: {lax: {harness: opencode-openrouter, model: m, sandbox: lxx}}}\nsu2: {kind: suite, items: {su2: {agents: lax, tasks: txx, scorers: scx}}}\n", "build is a dockerfile"},
		{"raw config under lxd", base + "lxx: {kind: sandbox, items: {lxx: {driver: lxd, config: {raw.lxc: x}}}}\nlax: {kind: agent, items: {lax: {harness: opencode-openrouter, model: m, sandbox: lxx}}}\nsu2: {kind: suite, items: {su2: {agents: lax, tasks: txx, scorers: scx}}}\n", "config raw.lxc is refused"},
		{"bad size", base + "dxx: {kind: sandbox, items: {dxx: {limits: {memory: lots}}}}\n", "limits.memory \"lots\" is not a size"},
		{"task sudo under docker", base + "t3x: {kind: task, items: {t3x: {prompt: p, sudo: true}}}\nsu3: {kind: suite, items: {su3: {agents: axx, tasks: t3x, scorers: scx}}}\n", "task \"t3x\" sets sudo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseT(t, tc.src)
			assert.Error(t, c.Validate(), tc.want)
		})
	}
}

// an agent scorer takes a task's fields, and synthesises the task its judge runs.
func TestValidateAgentScorer(t *testing.T) {
	c := parseT(t, base+`
jxx: {kind: agent, items: {jxx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
jud: {kind: scorer, items: {jud: {agent: jxx, prompt-file: r.md, stage: echo s, prepare: echo p, collect: echo c, env: {K: v}}}}
su2: {kind: suite, items: {su2: {agents: axx, tasks: txx, scorers: jud}}}
`)
	require.NoError(t, c.Validate())
	var sc Scorer
	for _, x := range c.Scorers {
		if x.Name == "jud" {
			sc = x
		}
	}
	task := sc.Task()
	assert.Equal(t, task.Name, "jud")
	assert.Equal(t, task.Stage, "echo s")
	assert.Equal(t, task.Env["K"], "v")
	assert.Equal(t, task.TimeoutDuration(), DefaultJudgeTimeout)
}

// an lxd sandbox takes its own keys, and the driver-neutral ones.
func TestValidateLXDSandbox(t *testing.T) {
	c := parseT(t, base+`lxx: {kind: sandbox, items: {lxx: {driver: lxd, remote: armstrong, vm: true, base: ubuntu:26.04, provision: "echo hi", sudo: true, max-parallel: 2, limits: {cpu: 4, memory: 8GiB, disk: 20GiB}, profiles: [default], config: {security.nesting: "true"}}}}
lax: {kind: agent, items: {lax: {harness: opencode-openrouter, model: m, sandbox: lxx}}}
su2: {kind: suite, items: {su2: {agents: lax, tasks: txx, scorers: scx}}}
`)
	require.NoError(t, c.Validate())
	var lx Sandbox
	for _, sb := range c.Sandboxes {
		if sb.Name == "lxx" {
			lx = sb
		}
	}
	assert.Equal(t, lx.Limits.Memory, "8GiB")
	assert.Equal(t, lx.Config["security.nesting"], "true")
	assert.Equal(t, lx.VM, true)
}

func TestValidateBwrapSandbox(t *testing.T) {
	c := parseT(t, base+`bwx: {kind: sandbox, items: {bwx: {driver: bwrap, image: ghcr.io/x/y:1, max-parallel: 2, env: {A: b}, egress: {allow: [.ubuntu.com]}}}}
bax: {kind: agent, items: {bax: {harness: opencode-openrouter, model: m, sandbox: bwx}}}
su2: {kind: suite, items: {su2: {agents: bax, tasks: txx, scorers: scx}}}
`)
	require.NoError(t, c.Validate())
}

func TestValidateHostSandbox(t *testing.T) {
	c := parseT(t, base+`hxx: {kind: sandbox, items: {hxx: {driver: host, sand-not-boxed: true, max-parallel: 2, env: {A: b}, egress: {allow: [.ubuntu.com]}}}}
hax: {kind: agent, items: {hax: {harness: opencode-openrouter, model: m, sandbox: hxx}}}
su2: {kind: suite, items: {su2: {agents: hax, tasks: txx, scorers: scx}}}
`)
	require.NoError(t, c.Validate())
}

func TestValidateAccepts(t *testing.T) {
	cases := []struct{ name, src string }{
		{"base", base},
		{"task and scorer share an id (different kinds)", base + "xxx: {kind: task, items: {same: {prompt: p}}}\nsc2: {kind: scorer, items: {same: {score: \"echo 1\"}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: same, scorers: same}}}\n"},
		{"singleton vector named after its item", base + "lone: {kind: task, items: {lone: {prompt: p}}}\nsu2: {kind: suite, items: {su2: {agents: axx, tasks: lone}}}\n"},
		{"sandbox via a vector of one", base + "bxx: {kind: agent, items: {bxx: {harness: claude-cli-keyless, model: m, sandbox: sxx}}}\nsu2: {kind: suite, items: {su2: {agents: bxx, tasks: txx}}}\n"},
		{"codex with effort", base + "bxx: {kind: agent, items: {bxx: {harness: codex-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: bxx, tasks: txx}}}\n"},
		{"copilot with effort", base + "bxx: {kind: agent, items: {bxx: {harness: copilot-cli-keyless, model: m, effort: high}}}\nsu2: {kind: suite, items: {su2: {agents: bxx, tasks: txx}}}\n"},
		{"sandbox with neither image nor build (the published default)", "sxx: {kind: sandbox, items: {sxx: {driver: docker}}}\n"},
		{"egress allowlist", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {allow: [good.com, 'github.com:22', '10.0.0.5:5432']}}}}\n"},
		{"egress denylist", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {default: allow, deny: [evil.com]}}}}\n"},
		{"egress url allow under default allow", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {default: allow, allow: ['a.test/public*']}}}}\n"},
		{"egress url entries and a regexp", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {allow: ['.githubusercontent.com/canonical/*', 'api.github.com ~^/repos/'], deny: ['github.com/*/x*']}}}}\n"},
		{"task egress under a proxy sandbox", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {allow: [a.test]}}}}\naxx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}}}\ntxx: {kind: task, items: {txx: {prompt: p, egress: {allow: [pypi.org], deny: ['github.com/*/answer*']}}}}\nsux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}\n"},
		{"egress image under podman", "sxx: {kind: sandbox, items: {sxx: {driver: podman, image: img, egress: {image: p}}}}\n"},
		{"sandbox env", "sxx: {kind: sandbox, items: {sxx: {image: img, env: {DENO_CERT: /sbx-ca/bundle.pem}}}}\n"},
		{"egress methods posture", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {methods: [GET, HEAD], allow: [a.test, 'POST a.test/graphql'], deny: ['PUT,PATCH a.test/*']}}}}\n"},
		{"egress none with no agents", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: none}}}\n"},
		{"egress empty mapping is a proxy", "sxx: {kind: sandbox, items: {sxx: {image: img, egress: {}}}}\n"},
		{"run-only suite", base + "su2: {kind: suite, items: {su2: {agents: axx, tasks: txx}}}\n"},
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
	_, err := Parse([]byte("sxx: {kind: sandbox, items: {sxx: {image: img, egress: {mode: proxy}}}}\n"))
	assert.Error(t, err, "egress: unknown field \"mode\"")
	_, err = Parse([]byte("sxx: {kind: sandbox, items: {sxx: {image: img, egress: open}}}\n"))
	assert.Error(t, err, "want none or a mapping")
	// a task carries no posture: that is the sandbox's.
	_, err = Parse([]byte("xxx: {kind: task, items: {xxx: {prompt: p, egress: {default: allow}}}}\n"))
	assert.Error(t, err, "unknown field \"default\"")
	_, err = Parse([]byte("xxx: {kind: task, items: {xxx: {prompt: p, egress: none}}}\n"))
	assert.Error(t, err, "a task takes a mapping")
}

// the three shapes of `egress`: absent is open, the scalar none is no
// network, a mapping (even empty) is the proxy.
func TestEgressShapes(t *testing.T) {
	c := parseT(t, "sxx: {kind: sandbox, items: {open: {image: i}, off: {image: i, egress: none}, pxx: {image: i, egress: {allow: [a.test]}}, nulled: {image: i, egress: ~}}}\n")
	modes := map[string]string{}
	for _, sb := range c.Sandboxes {
		modes[sb.Name] = sb.Egress.Mode()
	}
	assert.Equal(t, modes["open"], "open")
	assert.Equal(t, modes["off"], "none")
	assert.Equal(t, modes["pxx"], "proxy")
	assert.Equal(t, modes["nulled"], "open")
}

// effortHarnesses is a subset view over Harnesses; pin it so a typo'd or
// removed harness can't linger there.
func TestEffortHarnessesAreHarnesses(t *testing.T) {
	for k := range effortHarnesses {
		assert.That(t, Harnesses[k], "effortHarnesses entry %q is not a harness", k)
	}
}

// FuzzParse: Parse never panics on arbitrary yaml, and what it loads Validate
// can always check. a config that validates clean is left as parsed and is
// clean again; it expands, every pair made of items it defines; every agent
// has its one sandbox; every name and vector name passes badName, no name
// twice in a kind; and no checked string holds a ${...} placeholder.
func FuzzParse(f *testing.F) {
	seed, err := os.ReadFile("../../pats.example.yaml")
	if err == nil {
		f.Add(seed)
	}
	f.Add([]byte("agents: [foo\n"))
	f.Add([]byte("xxx: {kind: task, items: {axx: {prompt-file: \"${nope}\"}}}\n"))
	f.Add([]byte(base))
	f.Add([]byte(base + "bxx: {kind: agent, items: {bxx: {harness: opencode-openrouter, model: m, max-cost: .nan}}}\nsu2: {kind: suite, items: {su2: {agents: [axx, bxx], tasks: txx}}}\n"))
	f.Add([]byte("sdf: {kind: scorer, defaults: &py {score: ./s.py}, items: {one: ~, two: ~}}\nmore: {kind: scorer, defaults: {<<: *py, score: ~}, items: {three: {score: ./t.py}}}\n"))
	f.Add([]byte("sxx: {kind: sandbox, items: {sxx: {image: img, env: {A: b}, egress: {methods: [GET], allow: [a.test, 'POST a.test/graphql'], deny: ['github.com/*/x*']}}}}\n"))
	f.Add([]byte("xxx: {kind: task, defaults: {prompt: p, timeout: 5m, env: {MODE: knockout}}, items: {axx: ~, bxx: {env: {MODE: ~}, egress: {allow: [c.test]}}}}\n"))
	f.Add([]byte("xxx: {kind: task, dir: cases}\n"))
	f.Add([]byte("xxx: &x {kind: task, items: {axx: *x}}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil || c.Validate() != nil {
			return
		}

		// a second copy, parsed afresh, is what Validate must leave c as.
		fresh, err := Parse(data)
		require.NoError(t, err)
		assert.That(t, reflect.DeepEqual(c, fresh), "Validate changed the config:\n%+v\nwas\n%+v", c, fresh)
		assert.NoError(t, c.Validate(), "clean once, not twice")

		defined := map[string]map[string]bool{}
		for _, kind := range Kinds {
			defined[kind] = map[string]bool{}
			for _, m := range c.members(kind) {
				assert.Equal(t, badName(m.name), "", "%s %q", kind, m.name)
				assert.Equal(t, badName(m.vector), "", "%s vector %q", kind, m.vector)
				assert.That(t, !defined[kind][m.name], "%s %q is defined twice", kind, m.name)
				defined[kind][m.name] = true
			}
		}

		pairs, err := c.ExpandTestPairs()
		require.NoError(t, err)
		for _, p := range pairs {
			assert.That(t, defined["agent"][p.Agent] && defined["task"][p.Task], "pair %v names an item the config lacks", p)
		}
		scored, err := c.ExpandScorePairs()
		require.NoError(t, err)
		for _, p := range scored {
			assert.That(t, defined["task"][p.Task] && defined["scorer"][p.Scorer], "score pair %v names an item the config lacks", p)
		}
		for _, a := range c.Agents {
			sb, err := c.ResolveSandbox(a)
			require.NoError(t, err)
			assert.That(t, defined["sandbox"][sb], "agent %q resolves to sandbox %q, not defined", a.Name, sb)
		}

		for i := range c.Sandboxes {
			assert.NoError(t, checkTemplates(&c.Sandboxes[i], "sandbox"))
		}
		for i := range c.Agents {
			assert.NoError(t, checkTemplates(&c.Agents[i], "agent"))
		}
		for i := range c.Tasks {
			assert.NoError(t, checkTemplates(&c.Tasks[i], "task"))
		}
		for i := range c.Scorers {
			assert.NoError(t, checkTemplates(&c.Scorers[i], "scorer"))
		}
	})
}

// FuzzCheckTemplates: nothing in pats.yaml is expanded, and the five ${...}
// placeholders are refused. a checked string is refused exactly when it
// holds one, wherever it sits in the item; a prompt is never refused and
// comes back from Parse byte for byte; a suite holds references, not text.
func FuzzCheckTemplates(f *testing.F) {
	for _, s := range []string{
		"", "p/${task}.txt", "${agent}${agent}", "${vector", "$task", "${Task}", "${ task }", "${nope}", "$${scorer}}",
		"${${sandbox}}", "a$b", "\x00${task}", "tasks/\xc3\xa9/${vector}\n", "${task\n}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		held := false
		for _, name := range []string{"task", "agent", "scorer", "sandbox", "vector"} {
			held = held || strings.Contains(s, "${"+name+"}")
		}

		for kind, item := range map[string]any{
			"task": &Task{PromptFile: s}, "scorer": &Scorer{Score: s}, "agent": &Agent{Model: s}, "sandbox": &Sandbox{Image: s},
		} {
			assert.Equal(t, checkTemplates(item, kind) != nil, held, "%s holding %q", kind, s)
		}
		// nested: a map's value, a list's entry inside an embedded struct.
		assert.Equal(t, checkTemplates(&Task{Env: map[string]string{"K": s}}, "task") != nil, held, "env %q", s)
		assert.Equal(t, checkTemplates(&Sandbox{Egress: Egress{Policy: proxy.Policy{Allow: []string{"a.test", s}}}}, "sandbox") != nil, held, "egress %q", s)

		assert.NoError(t, checkTemplates(&Task{Prompt: s}, "task"))
		assert.NoError(t, checkTemplates(&Scorer{Prompt: s, Agent: "jxx"}, "scorer"))
		assert.NoError(t, checkTemplates(&Suite{Agents: StrList{s}}, "suite"))

		// through Parse: the same string as a yaml double-quoted scalar.
		if !utf8.ValidString(s) {
			return
		}
		q := strconv.QuoteToASCII(s)
		_, err := Parse([]byte("xxx: {kind: task, items: {axx: {prompt-file: " + q + "}}}\n"))
		assert.Equal(t, err != nil, held, "Parse of prompt-file %s: %v", q, err)
		c, err := Parse([]byte("xxx: {kind: task, items: {axx: {prompt: " + q + "}}}\n"))
		require.NoError(t, err, "Parse of prompt %s", q)
		require.Len(t, c.Tasks, 1)
		assert.Equal(t, c.Tasks[0].Prompt, s)
	})
}

// task egress follows the vectors rule: an item's block replaces the
// defaults' whole, and a null drops it.
func TestTaskEgressDefaults(t *testing.T) {
	c := parseT(t, `xxx:
  kind: task
  defaults: {prompt: p, egress: {deny: ['a.test/x*'], allow: [b.test]}}
  items:
    inherits: ~
    replaces: {egress: {allow: [c.test]}}
    drops: {egress: ~}
`)
	got := map[string]TaskEgress{}
	for _, tk := range c.Tasks {
		got[tk.Name] = tk.Egress
	}
	assert.EqualArrays(t, got["inherits"].Deny, []string{"a.test/x*"})
	assert.EqualArrays(t, got["replaces"].Allow, []string{"c.test"})
	assert.Equal(t, len(got["replaces"].Deny), 0)
	assert.Equal(t, got["drops"].Set, false)
}

// a name is chisel's package name: debian's own names fit, a dot never leads.
func TestBadName(t *testing.T) {
	for _, ok := range []string{"curl", "python3.14", "libstdc++6", "g++-14", "0ad", "ab", "base-passwd"} {
		assert.Equal(t, badName(ok), "", ok)
	}
	for _, bad := range []string{"", "a", ".judge", "-ab", "ab-", "a--b", "Ab", "a_b", "a/b", "a b", "+ab"} {
		assert.That(t, badName(bad) != "", "%q passed", bad)
	}
}
