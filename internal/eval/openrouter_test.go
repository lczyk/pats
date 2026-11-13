package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// the check hands back what the catalogue says each agent's model costs. a
// cache price a model leaves out is its prompt's, and a model with no price
// it can read has none.
func TestModelPrices(t *testing.T) {
	catalogueServer(t, 200, `{"data": [
	{"id": "x/full", "supported_parameters": ["tools"], "pricing": {"prompt": "0.000002", "completion": "0.000008", "input_cache_read": "0.0000005", "input_cache_write": "0.0000025"}},
	{"id": "x/plain", "supported_parameters": ["tools"], "pricing": {"prompt": "0.000001", "completion": "0.000003"}},
	{"id": "x/free", "supported_parameters": ["tools"], "pricing": {"prompt": "0", "completion": "0"}},
	{"id": "x/none", "supported_parameters": ["tools"]},
	{"id": "x/odd", "supported_parameters": ["tools"], "pricing": {"prompt": "-1", "completion": "cheap"}}
]}`)
	agents := []config.Agent{orAgent("full", "x/full:nitro"), orAgent("plain", "x/plain"), orAgent("free", "x/free"), orAgent("none", "x/none"), orAgent("odd", "x/odd")}
	prices, _, err := checkOpenrouterModels(context.Background(), agents, http.DefaultClient)
	require.NoError(t, err)
	assert.Equal(t, *prices["full"], modelPrice{in: 2e-6, out: 8e-6, cacheRead: 5e-7, cacheWrite: 2.5e-6})
	assert.Equal(t, *prices["plain"], modelPrice{in: 1e-6, out: 3e-6, cacheRead: 1e-6, cacheWrite: 1e-6})
	assert.Equal(t, *prices["free"], modelPrice{})
	assert.That(t, prices["none"] == nil && prices["odd"] == nil, "a model with no readable price has one")

	c, ok := prices["full"].cost(1000, 100, 2000, 400)
	assert.Equal(t, ok, true)
	assert.NearlyEqual(t, c, 0.002+0.0008+0.001+0.001, 1e-12)
	_, ok = prices["full"].cost(0, 0, 0, 0)
	assert.Equal(t, ok, false) // nothing to price is no cost, not a cost of 0
	_, ok = prices["none"].cost(1, 1, 0, 0)
	assert.Equal(t, ok, false)
	c, ok = prices["free"].cost(1, 1, 0, 0)
	assert.That(t, ok && c == 0, "a free model's tokens cost a real zero")
}

func orAgent(id, model string) config.Agent {
	return config.Agent{Name: id, Harness: "opencode-openrouter", Model: model}
}

// a model is checked by its catalogue id: exact, a catalogue variant only
// itself, routing shortcuts dropped wherever they sit; a missing one names
// the closest ids; one without tools fails; other harnesses are not checked.
func TestCheckOpenrouterModels(t *testing.T) {
	catalogueServer(t, 200, catalogue)
	ok := []config.Agent{orAgent("a", "openai/gpt-4o-mini"), orAgent("b", "x/y:free"), orAgent("c", "x/y:nitro:free"),
		orAgent("d", "x/y:free:floor:nitro"), orAgent("e", "openai/gpt-4o:nitro"),
		{Name: "claude", Harness: "claude-cli-keyless", Model: "nonsense"}}
	_, warns, err := checkOpenrouterModels(context.Background(), catalogueAgents(&config.Config{}, ok, "openrouter"), http.DefaultClient)
	require.NoError(t, err)
	assert.Len(t, warns, 0)

	_, _, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("typo", "openai/gpt-4o-mni")}, http.DefaultClient)
	assert.Error(t, err, `agent "typo": model "openai/gpt-4o-mni" is not on openrouter; closest: openai/gpt-4o-mini`)
	_, _, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("p", "prose/only")}, http.DefaultClient)
	assert.Error(t, err, "does not take tools")
	_, _, err = checkOpenrouterModels(context.Background(), []config.Agent{orAgent("u", "x/y:weird")}, http.DefaultClient)
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
		_, warns, err := checkOpenrouterModels(context.Background(), agents, http.DefaultClient)
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
	_, _, err := checkOpenrouterModels(ctx, agents, http.DefaultClient)
	assert.ErrorIs(t, err, context.Canceled)
}

// cannedBody answers every request with 200 and one body, without a socket.
type cannedBody []byte

func (b cannedBody) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Request: r,
		Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// FuzzCheckOpenrouterModels: the catalogue is a remote api's reply. a body
// that is no catalogue is one warning, never an error; one that is fails the
// agent exactly when its model's catalogue id is not listed or takes no
// tools, and offers at most three of its ids as closest, nearest first.
func FuzzCheckOpenrouterModels(f *testing.F) {
	for _, model := range []string{"openai/gpt-4o-mini", "openai/gpt-4o-mni", "x/y:nitro:free", "prose/only", "x/y:weird", "", ":nitro"} {
		f.Add([]byte(catalogue), model)
	}
	f.Add([]byte(`{}`), "x/y")
	f.Add([]byte(`not json`), "x/y")
	f.Add([]byte(`{"data":[]}`), "x/y")
	f.Add([]byte(`{"data":null}`), "x/y")
	f.Add([]byte(`{"data":[null,{"id":"x/y"},{"id":"x/y","supported_parameters":["tools"]}]} trailing`), "x/y:floor")
	f.Add([]byte(`{"data":[{"id":7}]}`), "x/y")

	f.Fuzz(func(t *testing.T, body []byte, model string) {
		id := catalogueID(model)
		assert.Equal(t, catalogueID(id), id, "the catalogue id of %q has one of its own", model)
		if !strings.Contains(model, ":") {
			assert.Equal(t, id, model)
		}

		client := &http.Client{Transport: cannedBody(body)}
		_, warns, err := checkOpenrouterModels(context.Background(), []config.Agent{orAgent("axx", model)}, client)

		// the catalogue as its reader takes it: the first json value of the body.
		var cat struct {
			Data []openrouterModel `json:"data"`
		}
		if json.NewDecoder(bytes.NewReader(body)).Decode(&cat) != nil || cat.Data == nil {
			require.NoError(t, err, "an unreadable catalogue failed the run")
			require.Len(t, warns, 1)
			assert.That(t, strings.HasPrefix(warns[0], "model check skipped: "), "%s", warns[0])
			return
		}
		assert.Len(t, warns, 0)

		listed, tools := 0, false
		for _, m := range cat.Data {
			if m.ID == id {
				listed++
				tools = slices.Contains(m.SupportedParameters, "tools")
			}
			d := editDistance(id, m.ID)
			assert.Equal(t, editDistance(m.ID, id), d, "distance %q to %q is not symmetric", id, m.ID)
			assert.Equal(t, d == 0, m.ID == id, "distance %d between %q and %q", d, id, m.ID)
			assert.That(t, d >= max(len(id), len(m.ID))-min(len(id), len(m.ID)) && d <= max(len(id), len(m.ID)),
				"distance %d between %q and %q", d, id, m.ID)
		}
		switch {
		case listed == 0:
			assert.Error(t, err, "is not on openrouter")
		case listed > 1: // an id listed twice: which entry speaks is not pinned here
		case !tools:
			assert.Error(t, err, "does not take tools")
		default:
			assert.NoError(t, err)
		}

		near := closestIDs(id, cat.Data, 3)
		assert.Len(t, near, min(3, len(cat.Data)))
		for i, n := range near {
			assert.That(t, slices.ContainsFunc(cat.Data, func(m openrouterModel) bool { return m.ID == n }), "closest %q is not in the catalogue", n)
			if i > 0 {
				assert.That(t, editDistance(id, near[i-1]) <= editDistance(id, n), "closest ids out of order: %q before %q", near[i-1], n)
			}
		}
		for _, m := range cat.Data {
			if len(near) > 0 {
				assert.That(t, editDistance(id, m.ID) >= editDistance(id, near[0]), "%q is closer than the closest %q", m.ID, near[0])
			}
		}
	})
}

// a bad model stops the run before the run dir: nothing is made.
func TestRunModelCheckBeforeRunDir(t *testing.T) {
	old := checkModels
	checkModels = checkOpenrouterModels
	defer func() { checkModels = old }()
	catalogueServer(t, 200, catalogue)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: openai/gpt-4o-mni, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
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

// ctrl-c while the catalogue loads is an interrupted run: no run dir.
func TestRunModelCheckInterrupted(t *testing.T) {
	old := checkModels
	checkModels = checkOpenrouterModels
	defer func() { checkModels = old }()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	t.Setenv("PATS_OPENROUTER_URL", slow.URL)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	go func() { time.Sleep(200 * time.Millisecond); _ = syscall.Kill(os.Getpid(), syscall.SIGINT) }()
	var out strings.Builder
	_, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	assert.ErrorIs(t, err, ErrInterrupted)
	_, statErr := os.Stat(filepath.Join(dir, runsSubdir))
	assert.That(t, os.IsNotExist(statErr), "a run dir was made")
}

// the catalogue's agents are the ones whose harness names it and that have a
// model to look up.
func TestCatalogueAgents(t *testing.T) {
	cfg := &config.Config{}
	got := catalogueAgents(cfg, []config.Agent{
		orAgent("with", "openai/gpt-4o-mini"),
		orAgent("without", ""),
		{Name: "claude", Harness: "claude-cli-keyless", Model: "m"},
		{Name: "unknown", Harness: "nope", Model: "m"},
	}, "openrouter")
	require.Len(t, got, 1)
	assert.Equal(t, got[0].Name, "with")
}
