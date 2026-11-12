package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
)

// parseCfg loads a pats.yaml body for a test, validated.
func parseCfg(t *testing.T, src string, machineFile ...string) *config.Config {
	t.Helper()
	if len(machineFile) > 0 {
		return loadCfg(t, src, machineFile[0])
	}
	cfg, err := config.Parse([]byte(src))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	return cfg
}

// loadCfg is parseCfg on a machine with a config file of its own: the
// pats.yaml is read from a file, as the cli reads one, over that machine's
// harnesses.
func loadCfg(t *testing.T, src, machineFile string) *config.Config {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	require.NoError(t, os.MkdirAll(filepath.Join(xdg, "pats"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(xdg, "pats", "config.yaml"), []byte(machineFile), 0o644))
	path := filepath.Join(t.TempDir(), "pats.yaml")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	return cfg
}
