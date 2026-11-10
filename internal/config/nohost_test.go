//go:build nohost

package config

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
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
