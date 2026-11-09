package eval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
)

const catalogue = `{"data": [
	{"id": "openai/gpt-4o-mini", "supported_parameters": ["tools", "temperature"]},
	{"id": "openai/gpt-4o", "supported_parameters": ["tools"]},
	{"id": "x/y:free", "supported_parameters": ["tools"]},
	{"id": "x/y", "supported_parameters": ["tools"]},
	{"id": "prose/only", "supported_parameters": ["temperature"]}
]}`

func catalogueServer(t *testing.T, status int, body string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PATS_OPENROUTER_URL", srv.URL)
}

func orAgent(id, model string) config.Agent {
	return config.Agent{ID: id, Harness: "opencode-openrouter", Model: model}
}

// a model is checked by its catalogue id: exact, a catalogue variant only
// itself, routing shortcuts dropped wherever they sit; a missing one names
// the closest ids; one without tools fails; other harnesses are not checked.
func TestCheckOpenrouterModels(t *testing.T) {
	catalogueServer(t, 200, catalogue)
	ok := []config.Agent{orAgent("a", "openai/gpt-4o-mini"), orAgent("b", "x/y:free"), orAgent("c", "x/y:nitro:free"),
		orAgent("d", "x/y:free:floor:nitro"), orAgent("e", "openai/gpt-4o:nitro"),
		{ID: "claude", Harness: "claude-cli-keyless", Model: "nonsense"}}
	warns, err := checkOpenrouterModels(context.Background(), ok, http.DefaultClient)
	require.NoError(t, err)
	assert.Len(t, warns, 0)

	_, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("typo", "openai/gpt-4o-mni")}, http.DefaultClient)
	assert.Error(t, err, `agent "typo": model "openai/gpt-4o-mni" is not on openrouter; closest: openai/gpt-4o-mini`)
	_, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("p", "prose/only")}, http.DefaultClient)
	assert.Error(t, err, "does not take tools")
	_, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("u", "x/y:weird")}, http.DefaultClient)
	assert.Error(t, err, `model "x/y:weird" is not on openrouter`)
}

// a catalogue that cannot be read is a warning and the run goes on; a
// cancelled context is not swallowed.
func TestCheckOpenrouterModelsUnreadable(t *testing.T) {
	agents := []config.Agent{orAgent("a", "openai/gpt-4o-mini")}
	for _, c := range []struct {
		status int
		body   string
	}{{500, "oops"}, {200, "{}"}, {200, "not json"}} {
		catalogueServer(t, c.status, c.body)
		warns, err := checkOpenrouterModels(context.Background(), agents, http.DefaultClient)
		require.NoError(t, err, c.body)
		require.Len(t, warns, 1)
		assert.That(t, strings.HasPrefix(warns[0], "model check skipped: "), "%s", warns[0])
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	t.Setenv("PATS_OPENROUTER_URL", slow.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := checkOpenrouterModels(ctx, agents, http.DefaultClient)
	assert.ErrorIs(t, err, context.Canceled)
}

// a bad model stops the run before the run dir: nothing is made.
func TestRunModelCheckBeforeRunDir(t *testing.T) {
	old := checkModels
	checkModels = checkOpenrouterModels
	defer func() { checkModels = old }()
	catalogueServer(t, 200, catalogue)
	dir := t.TempDir()
	cfg := parseCfg(t, `
s: {kind: sandbox, items: {s: {image: s}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: openai/gpt-4o-mni, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
`)
	pinAll(t, dir, cfg)
	var out strings.Builder
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assert.Error(t, err, "is not on openrouter")
	_, statErr := os.Stat(filepath.Join(dir, runsSubdir))
	assert.That(t, os.IsNotExist(statErr), "a run dir was made")
	// --offline skips the check.
	_, err = Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out, Offline: true})
	assert.That(t, err == nil || !strings.Contains(err.Error(), "openrouter"), "offline still checked: %v", err)
}
