package config

import (
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// steps is base with a tools sandbox the step-sandbox rules can name.
const steps = `
sxx: {kind: sandbox, items: {sxx: {image: img}}}
too: {kind: sandbox, items: {too: {image: tools}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx, scorers: scx}}}
`

func TestStepSandboxParsed(t *testing.T) {
	c := parseT(t, steps+`txx: {kind: task, defaults: {step-sandbox: too, step-env: TOKEN}, items: {txx: {prompt: p, stage: ./s}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py, step-sandbox: too, step-env: [A, B]}}}
`)
	require.NoError(t, c.Validate())
	assert.Equal(t, c.Tasks[0].StepSandbox, "too")
	assert.EqualArrays(t, c.Tasks[0].StepEnv, []string{"TOKEN"})
	assert.EqualArrays(t, c.Scorers[0].StepEnv, []string{"A", "B"})
}

// an agent scorer's steps reach its judge's task with their placement.
func TestStepSandboxJudgeTask(t *testing.T) {
	s := Scorer{Name: "j", Agent: "axx", Stage: "./s", StepSandbox: "too", StepEnv: StrList{"T"}}
	task := s.Task()
	assert.Equal(t, task.StepSandbox, "too")
	assert.EqualArrays(t, task.StepEnv, []string{"T"})
}

func TestStepSandboxRejects(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown sandbox", `txx: {kind: task, items: {txx: {prompt: p, stage: ./s, step-sandbox: nope}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-sandbox: unknown sandbox "nope"`},
		{"step-env alone", `txx: {kind: task, items: {txx: {prompt: p, step-env: [T]}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env needs a step-sandbox`},
		{"step-env PATS_X", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: [PATS_X]}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env "PATS_X": PATS_* is reserved for pats`},
		{"step-env PATH", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: [PATH]}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env PATH is set by pats in the box`},
		{"step-env HOME", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: [HOME]}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env HOME is set by pats in the box`},
		{"step-env 1x", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: ['1x']}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env "1x" is not a valid variable name`},
		{"step-env UID", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: [UID]}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": step-env UID is read-only in a shell`},
		{"step-env and env", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, step-env: [T], env: {T: v}}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": T is in both env and step-env`},
		{"HOME beside a step sandbox", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, env: {HOME: /x}}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `task "txx": env HOME: a step sandbox sets HOME itself`},
		{"max-parallel on a steps-only sandbox", `txx: {kind: task, items: {txx: {prompt: p, step-sandbox: mpx}}}
mpx: {kind: sandbox, items: {mpx: {image: i, max-parallel: 2}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`, `sandbox "mpx": max-parallel bounds agents`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseT(t, steps+tc.src).Validate()
			assert.Error(t, err, tc.want)
		})
	}
}
