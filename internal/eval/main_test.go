package eval

import (
	"os"
	"testing"
)

// every test's init registers its project under a cache dir of its own,
// never the user's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pats-eval-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
