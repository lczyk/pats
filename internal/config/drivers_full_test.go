//go:build !nohost && !nobwrap && !nopodman

package config

import (
	"slices"
	"testing"

	"github.com/lczyk/assert"

	"github.com/lczyk/pats/src/sandbox"
)

// a full build has every driver config knows, and no other.
func TestKnownDriversBuilt(t *testing.T) {
	built := sandbox.Drivers()
	for d := range knownDrivers {
		assert.That(t, slices.Contains(built, d), "driver %q known to config, not built", d)
	}
	assert.Equal(t, len(built), len(knownDrivers))
}
