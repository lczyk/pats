//go:build nohost

package config

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a build without the host driver names it as left out, not as unknown.
func TestHostNotInThisBuild(t *testing.T) {
	c := parseT(t, base+`hxx: {kind: sandbox, items: {hxx: {driver: host, sand-not-boxed: true}}}
hax: {kind: agent, items: {hax: {harness: opencode-openrouter, model: m, sandbox: hxx}}}
su2: {kind: suite, items: {su2: {agents: hax, tasks: txx, scorers: scx}}}
`)
	err := c.Validate()
	assert.Error(t, err, `sandbox "hxx": driver "host" is not in this build`)
	assert.That(t, !strings.Contains(err.Error(), "sand-not-boxed"), "sand-not-boxed reported too: %v", err)
}

// a build with no host runs no host step: each one unplaced is one line,
// with no advice, an agent scorer's steps included.
func TestHostStepsNeedASandbox(t *testing.T) {
	withBuiltHostSteps(t)
	err := parseT(t, steps+`txx: {kind: task, items: {txx: {prompt-cmd: ./p, stage: ./s, collect: ./c}}}
scs: {kind: scorer, items: {scx: {score: ./sc.py}, jud: {agent: axx, prompt: g, stage: ./s}}}
`).Validate()
	require.NotNil(t, err)
	var got []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if strings.Contains(l, "step-sandbox") {
			got = append(got, l)
		}
	}
	assert.Equal(t, strings.Join(got, "\n"), strings.Join([]string{
		`task "txx": stage needs a step-sandbox: this build runs no host steps`,
		`task "txx": prompt-cmd needs a step-sandbox: this build runs no host steps`,
		`task "txx": collect needs a step-sandbox: this build runs no host steps`,
		`scorer "scx": score needs a step-sandbox: this build runs no host steps`,
		`scorer "jud": stage needs a step-sandbox: this build runs no host steps`,
	}, "\n"))
}
