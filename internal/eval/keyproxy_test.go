package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// fixtureKey is split so it reads as a fixture, not a secret.
var fixtureKey = "sk-" + "or-v1-" + strings.Repeat("0123456789abcdef", 4)

// keyBox is a sandbox that records the specs it starts and, as each session
// closes, writes audit as the pair's proxy would have.
type keyBox struct {
	mu    sync.Mutex
	specs []sandbox.Spec
	envs  []map[string]string
	audit string // what a closing session's proxy leaves in its audit
	noLog bool   // the proxy leaves no audit at all
}

func (b *keyBox) Run(context.Context, sandbox.Spec, io.Writer, io.Writer) (int, error) { return 0, nil }

func (b *keyBox) Start(_ context.Context, spec sandbox.Spec) (sandbox.Session, error) {
	b.mu.Lock()
	b.specs = append(b.specs, spec)
	b.mu.Unlock()
	return &keySession{b: b, spec: spec}, nil
}

type keySession struct {
	b    *keyBox
	spec sandbox.Spec
	once sync.Once
}

func (s *keySession) Push(context.Context, string, string) error { return nil }
func (s *keySession) Pull(context.Context, string, string) error { return nil }
func (s *keySession) HandOver(context.Context, ...string) error  { return nil }
func (s *keySession) Sync(context.Context) error                 { return nil }

func (s *keySession) Close() error {
	s.once.Do(func() {
		if p := s.spec.Egress.AuditPath; p != "" && !s.b.noLog {
			_ = os.WriteFile(p, []byte(s.b.audit), 0o644)
		}
	})
	return nil
}

func (s *keySession) Exec(_ context.Context, cmd sandbox.Cmd, stdout, _ io.Writer) (int, error) {
	s.b.mu.Lock()
	s.b.envs = append(s.b.envs, cmd.Env)
	s.b.mu.Unlock()
	_, _ = io.WriteString(stdout, piLog)
	return 0, nil
}

const substituted = `{"ev":"req","host":"openrouter.ai","kind":"mitm","allowed":true,"rule":"POST openrouter.ai/api/v1/chat/completions","method":"POST","url":"openrouter.ai/api/v1/chat/completions","status":200,"key":"substituted"}` + "\n"

func keyCfg(t *testing.T, stage string) *config.Config {
	t.Helper()
	return parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s, egress: {key-proxy: true, allow: [x.test]}}}}
axx: {kind: agent, items: {axx: {harness: pi-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {stage: '`+stage+`',
  prompt-cmd: 'echo "p=${OPENROUTER_API_KEY:-none}" > "$PATS_OUTPUT_DIR/prompt-cmd.txt"; echo go',
  collect: 'echo "c=${OPENROUTER_API_KEY:-none}" > "$PATS_OUTPUT_DIR/collect.txt"'}}}
scx: {kind: scorer, items: {scx: {score: 'test -z "$OPENROUTER_API_KEY" && echo 1 || echo 0'}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx, scorers: scx}}}
`)
}

// what a host step wrote of the key, in the pair's dir.
func stepSaw(t *testing.T, pairDir, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pairDir, file))
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

// a key proxy's pair: the agent gets a placeholder of the key's shape, a
// fresh one per session; the proxy the real value and the binding; the host
// stage never inherits the key; the metadata records no value and counts
// what went out.
func TestKeyProxyPair(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", fixtureKey)
	box := &keyBox{audit: substituted}
	old := newSandbox
	newSandbox = func(sandbox.Options, sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := keyCfg(t, `echo "k=${OPENROUTER_API_KEY:-none}"`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Out: &out, Verbose: 2})
	require.NoError(t, err, out.String())

	require.Len(t, box.specs, 2) // the preflight, then the pair
	var placeholders []string
	for i, spec := range box.specs {
		ph := box.envs[i]["OPENROUTER_API_KEY"]
		assert.That(t, ph != fixtureKey && len(ph) == len(fixtureKey) && strings.HasPrefix(ph, "sk-"+"or-v1-"), "session %d got %q", i, ph)
		placeholders = append(placeholders, ph)
		assert.Equal(t, spec.Egress.Keys[ph], fixtureKey)
		require.Len(t, spec.Egress.Policy.Keys, 1)
		k := spec.Egress.Policy.Keys[0]
		assert.Equal(t, k.Name, "OPENROUTER_API_KEY")
		assert.Equal(t, k.Placeholder, ph)
		assert.EqualArrays(t, k.Bind, []string{"POST openrouter.ai/api/v1/chat/completions"})
		assert.That(t, strings.Contains(strings.Join(proxy.Matches(spec.Egress.Policy.Allow), " "), "POST openrouter.ai/api/v1/chat/completions"), "binding not allowed: %v", spec.Egress.Policy.Allow)
		for name, v := range spec.Env {
			assert.That(t, !strings.Contains(v, fixtureKey), "spec env %s holds the key", name)
		}
	}
	assert.That(t, placeholders[0] != placeholders[1], "preflight and pair share a placeholder")

	pairDir := filepath.Join(runDir, "axx", "txx")
	assert.Equal(t, stepSaw(t, pairDir, "stage.log"), "k=none")
	assert.Equal(t, stepSaw(t, pairDir, "prompt-cmd.txt"), "p=none")
	assert.Equal(t, stepSaw(t, pairDir, "collect.txt"), "c=none")
	// a host scorer of the pair does not inherit the key either.
	scored, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: runDir, Out: io.Discard})
	require.NoError(t, err)
	assert.Equal(t, scored.PerPair["axx/txx"], 1.0)
	meta, err := os.ReadFile(filepath.Join(pairDir, "metadata.json"))
	require.NoError(t, err)
	assert.That(t, !strings.Contains(string(meta), fixtureKey), "the key in metadata.json")
	assert.That(t, !strings.Contains(string(meta), placeholders[1]), "the placeholder in metadata.json")
	var m pairMeta
	require.NoError(t, json.Unmarshal(meta, &m))
	assert.Equal(t, m.Egress.KeysSubstituted, 1)
	assert.Equal(t, m.Egress.Policy.Keys[0].Placeholder, "<placeholder>")
	assert.That(t, !strings.Contains(out.String(), fixtureKey), "the key in pats's -vv output")
	_ = filepath.WalkDir(runDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			assert.That(t, !bytes.Contains(b, []byte(fixtureKey)), "the key in %s", p)
		}
		return nil
	})

	// without a key proxy the stage inherits the key, as before.
	cfg.Sandboxes[0].Egress.KeyProxy = false
	box.specs, box.envs = nil, nil
	runDir, err = Run(cfg, Options{ConfigDir: dir, Out: &out})
	require.NoError(t, err)
	assert.Equal(t, stepSaw(t, filepath.Join(runDir, "axx", "txx"), "stage.log"), "k="+fixtureKey)
	assert.Equal(t, box.envs[0]["OPENROUTER_API_KEY"], fixtureKey)
}

// the preflight under a key proxy proves a key went out through it: a
// misuse fails it, as do no substituted request and no audit at all.
func TestKeyProxyPreflight(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", fixtureKey)
	box := &keyBox{}
	old := newSandbox
	newSandbox = func(sandbox.Options, sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()
	dir := t.TempDir()
	cfg := keyCfg(t, "true")
	pre := func() error {
		var out bytes.Buffer
		return Preflight(context.Background(), cfg, Options{ConfigDir: dir, Out: &out}, []config.TestPair{{Agent: "axx", Task: "txx"}})
	}

	box.audit = substituted
	require.NoError(t, pre())
	box.audit = substituted + `{"ev":"req","host":"x.test","url":"x.test/<placeholder>","allowed":false,"rule":"key","reason":"misuse","status":403}` + "\n"
	assert.Error(t, pre(), "refused off its binding at x.test/<placeholder>")
	box.audit = ""
	assert.Error(t, pre(), "never authenticated")
	box.noLog = true
	assert.Error(t, pre(), "the preflight's audit")

	// with no key to hold there is nothing to have gone out.
	t.Setenv("OPENROUTER_API_KEY", "")
	box.noLog, box.audit = false, ""
	require.NoError(t, pre())
}

// a key's misuse is its own list, not a blocked egress; substitutions are
// counted; a scrub refusal is a failure of the way out.
func TestReadEgressAuditKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "egress.log")
	require.NoError(t, os.WriteFile(p, []byte(substituted+substituted+
		`{"ev":"req","host":"x.test","url":"x.test/<placeholder>","allowed":false,"rule":"key","reason":"misuse","status":403}`+"\n"+
		`{"ev":"req","host":"openrouter.ai","url":"openrouter.ai/api/v1/chat/completions","allowed":false,"reason":"key-scrub","status":502}`+"\n"), 0o644))
	a := readEgressAudit(p)
	assert.Equal(t, a.Substituted, 2)
	assert.EqualArrays(t, a.KeyMisuse, []string{"x.test/<placeholder>"})
	assert.EqualArrays(t, a.Failed, []string{"openrouter.ai/api/v1/chat/completions"})
	assert.Len(t, a.Blocked, 0)
}

// holdKeys swaps a bound key for a placeholder; an unset key is left out;
// a key too short to stand in for is an error that names it, not its value.
func TestHoldKeysEnv(t *testing.T) {
	cfg := keyCfg(t, "true")
	hd := cfg.HarnessOf(cfg.Agents[0])
	env := map[string]string{"OPENROUTER_API_KEY": fixtureKey, "OTHER": "x"}
	held, real, err := holdKeys(hd, env)
	require.NoError(t, err)
	require.Len(t, held, 1)
	assert.Equal(t, real[env["OPENROUTER_API_KEY"]], fixtureKey)
	assert.Equal(t, env["OTHER"], "x")

	held, real, err = holdKeys(hd, map[string]string{})
	require.NoError(t, err)
	assert.Len(t, held, 0)
	assert.Len(t, real, 0)

	_, _, err = holdKeys(hd, map[string]string{"OPENROUTER_API_KEY": "sk-short"})
	assert.Error(t, err, "OPENROUTER_API_KEY")
	assert.That(t, !strings.Contains(err.Error(), "sk-short"), "the value in the error: %v", err)
}

// the report names a pair whose agent tried to spend a held key elsewhere.
func TestReportKeyMisuse(t *testing.T) {
	var p reportPair
	p.summaryFields(&pairMeta{Status: "ok", KeyMisuse: []string{"x.test/<placeholder>"}})
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var back struct {
		KeyMisuse []string `json:"key_misuse"`
	}
	require.NoError(t, json.Unmarshal(b, &back))
	assert.EqualArrays(t, back.KeyMisuse, []string{"x.test/<placeholder>"})
}

// the host steps beside a key proxy lose every harness's keys.
func TestHostEnvironDrops(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "x")
	t.Setenv("KEEP_ME", "y")
	env := strings.Join(hostEnviron(t.TempDir(), nil, []string{"OPENROUTER_API_KEY"}), "\n")
	assert.That(t, !strings.Contains(env, "OPENROUTER_API_KEY="), "kept the key")
	assert.ContainsString(t, env, "KEEP_ME=y")
}
