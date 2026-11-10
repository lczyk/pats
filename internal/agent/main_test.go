package agent

import (
	"os"
	"testing"

	"github.com/lczyk/pats/src/sandbox"
)

// the tests that start a real sandbox hold a run lease while they do: it goes
// under a cache dir of the tests', never the user's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pats-agent-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	code := m.Run()
	sandbox.Shutdown()
	os.RemoveAll(dir)
	os.Exit(code)
}
