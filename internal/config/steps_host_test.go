//go:build !nohost

package config

import (
	"testing"

	"github.com/lczyk/assert"
)

// a step sandbox on the host would be a second way to run steps there.
func TestStepSandboxNotHost(t *testing.T) {
	err := parseT(t, steps+`hxx: {kind: sandbox, items: {hxx: {driver: host, sand-not-boxed: true}}}
txx: {kind: task, items: {txx: {prompt: p, stage: ./s, step-sandbox: hxx}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`).Validate()
	assert.Error(t, err, `task "txx": step-sandbox "hxx" is a host sandbox: leave step-sandbox unset`)
}

// a native build runs an unplaced step on the host, as before.
func TestStepUnsetRunsOnHost(t *testing.T) {
	assert.NoError(t, parseT(t, steps+`txx: {kind: task, items: {txx: {prompt: p, stage: ./s, collect: ./c}}}
scx: {kind: scorer, items: {scx: {score: ./sc.py}}}
`).Validate())
}
