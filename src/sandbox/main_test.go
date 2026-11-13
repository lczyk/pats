package sandbox

import (
	"os"
	"testing"
)

// testCacheEnv marks a process started under this package's tests.
const testCacheEnv = "PATS_SANDBOX_TEST_CACHE"

// what a test makes through the package's own process (a workdir, the lease
// that goes with it) lands under a cache dir of the tests', never the user's.
// a test binary a test runs as its helper keeps the cache dir it was handed.
func TestMain(m *testing.M) {
	if os.Getenv(testCacheEnv) != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "pats-sandbox-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	os.Setenv(testCacheEnv, dir)
	code := m.Run()
	Shutdown()
	os.RemoveAll(dir)
	os.Exit(code)
}
