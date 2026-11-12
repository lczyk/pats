package eval

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// every test's init registers its project under a cache dir of its own,
// never the user's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pats-eval-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	// nor does a test read the user's machine config.
	os.Setenv("XDG_CONFIG_HOME", dir)
	// no test fetches a harness cli: the artefact is where the cache would
	// have it, unread.
	fetchArtefact = func(_ context.Context, a sandbox.Artefact, _ *http.Client) (string, error) {
		return filepath.Join(dir, "harness", a.SHA256), nil
	}
	// no test reaches openrouter; the model check's own tests call it.
	checkModels = func(context.Context, []config.Agent, *http.Client) ([]string, error) { return nil, nil }
	// the fixtures run their steps on the host, in every build.
	config.AllowHostStepsInTests()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
