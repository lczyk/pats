package eval

import (
	"testing"

	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
)

// parseCfg loads a pats.yaml body for a test, validated.
func parseCfg(t *testing.T, src string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(src))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	return cfg
}
