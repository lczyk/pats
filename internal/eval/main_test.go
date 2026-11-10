package eval

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/lczyk/pats/internal/config"
)

// every test's init registers its project under a cache dir of its own,
// never the user's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pats-eval-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	// no test reaches openrouter; the model check's own tests call it.
	checkModels = func(context.Context, []config.Agent, *http.Client) ([]string, error) { return nil, nil }
	// the fixtures run their steps on the host, in every build.
	config.AllowHostStepsInTests()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
