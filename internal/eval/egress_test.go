package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
)

const egressCfg = `box: {kind: sandbox, items: {sxx: {image: img, egress: {allow: [a.test, '.ubuntu.com'], deny: ['a.test/secret**']}}, open: {image: img}}}
axx: {kind: agent, items: {axx: {harness: claude-cli-keyless, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p, egress: {allow: [pypi.org], deny: ['github.com/*/answer*']}}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`

func egressConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(egressCfg))
	require.NoError(t, err)
	require.NoError(t, c.Validate())
	return c
}

func TestEffectivePolicy(t *testing.T) {
	c := egressConfig(t)
	pol, err := EffectivePolicy(c, "sxx", "", "")
	require.NoError(t, err)
	assert.EqualArrays(t, pol.Allow, []string{"a.test", ".ubuntu.com"})

	pol, err = EffectivePolicy(c, "sxx", "txx", "axx")
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(pol.Allow, " "), "pypi.org")
	assert.ContainsString(t, strings.Join(pol.Allow, " "), ".anthropic.com")
	assert.EqualArrays(t, pol.Deny, []string{"a.test/secret**", "github.com/*/answer*"})

	for _, bad := range [][3]string{{"ghost", "", ""}, {"open", "", ""}, {"sxx", "ghost", ""}, {"sxx", "", "ghost"}} {
		_, err := EffectivePolicy(c, bad[0], bad[1], bad[2])
		assert.Error(t, err, assert.AnyError, bad[0]+bad[1]+bad[2])
	}
}

func TestCheckURL(t *testing.T) {
	c := egressConfig(t)
	pol, err := EffectivePolicy(c, "sxx", "txx", "axx")
	require.NoError(t, err)
	cases := []struct {
		url, want string
		ok        bool
	}{
		{"https://a.test/x", "allow rule=a.test  (mitm;", true},
		{"https://a.test/secret/1", "deny  rule=a.test/secret**  (mitm;", false},
		{"archive.ubuntu.com", "allow rule=.ubuntu.com  (tunnel;", true},
		{"http://archive.ubuntu.com/x.deb", "allow rule=.ubuntu.com  (plain http;", true},
		{"https://evil.test/", "deny  rule=default  (tunnel;", false},
		{"https://github.com/canonical/answer", "deny  rule=default", false}, // github is not allowed at all
		{"https://api.anthropic.com/v1", "allow rule=.anthropic.com", true},
		{"https://a.test:8443/", "deny  rule=default", false},
		{"https://a.test/%73ecret/1", "deny  rule=a.test/secret**", false}, // decoded, as the proxy sees it
		{"https://a.test", "allow rule=a.test  (mitm;", true},              // empty path is "/"
	}
	for _, tc := range cases {
		var out bytes.Buffer
		ok, err := CheckURL(pol, tc.url, "GET", true, nil, &out)
		require.NoError(t, err)
		assert.Equal(t, ok, tc.ok, tc.url)
		assert.ContainsString(t, out.String(), tc.want)
		assert.Equal(t, strings.Contains(out.String(), "harness hosts not merged"), false)
	}

	pol, _ = EffectivePolicy(c, "sxx", "", "")
	var out bytes.Buffer
	ok, err := CheckURL(pol, "https://api.anthropic.com/", "", false, nil, &out)
	require.NoError(t, err)
	assert.Equal(t, ok, false)
	assert.ContainsString(t, out.String(), "harness hosts not merged")

	_, err = CheckURL(pol, "https://", "GET", false, nil, &out)
	assert.Error(t, err, "bad url")

	// behind the machine's proxy: upstream or direct, the credential redacted.
	chain, err := proxy.NewChain("http://u:secret@proxy.corp:3128", "", ".ubuntu.com")
	require.NoError(t, err)
	out.Reset()
	_, err = CheckURL(pol, "https://evil.test/", "GET", false, chain, &out)
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), "via upstream http://u:xxxxx@proxy.corp:3128")
	out.Reset()
	_, err = CheckURL(pol, "https://archive.ubuntu.com/", "GET", false, chain, &out)
	require.NoError(t, err)
	assert.ContainsString(t, out.String(), "direct, not via the upstream")
}

const sampleAudit = `{"ts":"2026-11-06T10:00:00Z","ev":"open","host":"archive.ubuntu.com","port":"443","kind":"tunnel","allowed":true,"rule":"default"}
{"ts":"2026-11-06T10:00:01Z","ev":"close","host":"archive.ubuntu.com","port":"443","kind":"tunnel","allowed":true,"in":2048,"out":100}
{"ts":"2026-11-06T10:00:02Z","ev":"req","host":"pypi.org","port":"443","kind":"mitm","allowed":true,"rule":"default","method":"GET","url":"pypi.org/simple/","ua":"pip/24.0","status":200,"in":500}
{"ts":"2026-11-06T10:00:03Z","ev":"req","host":"gist.github.com","port":"443","kind":"mitm","allowed":true,"rule":"default","method":"POST","url":"gist.github.com/","ua":"curl/8.5.0","status":201}
{"ts":"2026-11-06T10:00:04Z","ev":"open","host":"api.anthropic.com","port":"443","kind":"tunnel","allowed":true,"rule":".anthropic.com"}
{"ts":"2026-11-06T10:00:05Z","ev":"open","host":"evil.test","port":"443","kind":"tunnel","allowed":false,"rule":"default"}
{"ts":"2026-11-06T10:00:06Z","ev":"req","host":"github.com","port":"443","kind":"mitm","allowed":false,"rule":"github.com/*/answer*","method":"GET","url":"github.com/c/answer"}
{"ts":"2026-11-06T10:00:07Z","ev":"open","host":"git.test","port":"22","kind":"raw","allowed":true,"rule":"default"}
`

func TestEgressLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "egress.log")
	require.NoError(t, os.WriteFile(p, []byte(sampleAudit), 0o644))

	var out bytes.Buffer
	require.NoError(t, EgressLog(p, false, nil, &out))
	s := out.String()
	assert.ContainsString(t, s, "TIME")
	assert.ContainsString(t, s, "POST gist.github.com/")
	assert.ContainsString(t, s, "2.1k") // the close line's bytes
	assert.ContainsString(t, s, "git.test:22")

	out.Reset()
	require.NoError(t, EgressLog(p, true, nil, &out))
	assert.ContainsString(t, out.String(), "evil.test")
	assert.Equal(t, strings.Contains(out.String(), "pypi.org"), false)
	assert.Equal(t, strings.Count(strings.TrimSpace(out.String()), "\n"), 2) // header + 2 refusals

	out.Reset()
	require.NoError(t, EgressLog(p, false, []string{"post,put"}, &out))
	assert.ContainsString(t, out.String(), "gist.github.com")
	assert.Equal(t, strings.Contains(out.String(), "pypi.org"), false)

	assert.Error(t, EgressLog(filepath.Join(t.TempDir(), "none"), false, nil, &out), assert.AnyError)
}

func writeRun(t *testing.T, audit string) string {
	t.Helper()
	dir := t.TempDir()
	run := filepath.Join(dir, runsSubdir, "001-20261106-test-run")
	require.NoError(t, os.MkdirAll(filepath.Join(run, "axx", "txx"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(run, "axx", "txx", "egress.log"), []byte(audit), 0o644))
	// the pair's record holds the policy it ran under, its harness's hosts with it.
	require.NoError(t, os.WriteFile(filepath.Join(run, "axx", "txx", "metadata.json"),
		[]byte(`{"agent":"axx","harness":"claude-cli-keyless","egress":{"policy":{"harness":[".anthropic.com",".claude.com"]}}}`), 0o644))
	return dir
}

// a suggestion is pasted into a config: whatever a host is spelt as, the
// block reads back as yaml, to the hosts that were reached.
func TestSuggestEgressReadsBack(t *testing.T) {
	hosts := []string{"plain.test", "port.test:8443", "12:30:45", "true", "null", "~", "1e3", "0x1f", "- dash", "a: b", "#hash",
		"*.star", "[::1]:22", "q'uote", `d"quote`, "%percent", "@at", "back`tick", "{brace}", "", " lead", "trail ", "tab\there", "new\nline", "\u00e9t\u00e9.test"}
	var audit strings.Builder
	for _, h := range hosts {
		line, err := json.Marshal(map[string]any{"ev": "open", "host": h, "port": "443", "kind": "tunnel", "allowed": true})
		require.NoError(t, err)
		audit.Write(append(line, '\n'))
	}
	// a user agent is the agent's to choose too, and is written in a comment.
	for _, ua := range []string{"curl/1\rdefault: allow", "curl/2\ndeny: ['*']", "curl/3\u2028x: y", "curl/4\x00"} {
		line, err := json.Marshal(map[string]any{"ev": "req", "host": "plain.test", "port": "443", "kind": "mitm", "allowed": true, "method": "POST", "ua": ua})
		require.NoError(t, err)
		audit.Write(append(line, '\n'))
	}
	var out bytes.Buffer
	require.NoError(t, SuggestEgress(writeRun(t, audit.String()), "", &out))
	var top map[string]any
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &top), "%s", out.String())
	assert.Len(t, top, 1, "a key beside egress: %s", out.String())
	var got struct {
		Egress struct {
			Allow []string `yaml:"allow"`
		} `yaml:"egress"`
	}
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &got), "%s", out.String())
	want := slices.Clone(hosts)
	slices.Sort(want)
	assert.EqualArrays(t, got.Egress.Allow, want, "%s", out.String())
}

func TestSuggestEgress(t *testing.T) {
	dir := writeRun(t, sampleAudit)
	var out bytes.Buffer
	require.NoError(t, SuggestEgress(dir, "", &out))
	s := out.String()
	for _, want := range []string{
		"egress:\n  allow:\n",
		"# tunnels (no user agent or method visible)\n    - archive.ubuntu.com\n    - evil.test\n    - git.test:22\n",
		"# curl/8.5.0\n    - gist.github.com\n",
		"# pip/24.0\n    - pypi.org\n",
		"# methods: unknown -- tunnels hide them",
		"# harness hosts, merged per agent by pats: api.anthropic.com",
	} {
		assert.ContainsString(t, s, want)
	}
	assert.Equal(t, strings.Contains(s, "github.com/c/answer"), false) // an explicit deny is not suggested

	// the harness's hosts are the ones the run recorded, not today's: a
	// pair with no recorded policy has none kept out of the suggestion.
	require.NoError(t, os.WriteFile(filepath.Join(dir, runsSubdir, "001-20261106-test-run", "axx", "txx", "metadata.json"),
		[]byte(`{"agent":"axx","harness":"claude-cli-keyless"}`), 0o644))
	out.Reset()
	require.NoError(t, SuggestEgress(dir, "", &out))
	assert.ContainsString(t, out.String(), "    - api.anthropic.com\n")
	assert.Equal(t, strings.Contains(out.String(), "# harness hosts"), false)

	// tunnels only: methods are unknown, and said to be.
	dir = writeRun(t, `{"ev":"open","host":"a.test","port":"443","kind":"tunnel","allowed":true,"rule":"default"}
`)
	out.Reset()
	require.NoError(t, SuggestEgress(dir, "", &out))
	assert.ContainsString(t, out.String(), "methods: unknown")

	// requests only, all reads: methods is proposed.
	dir = writeRun(t, `{"ev":"req","host":"pypi.org","port":"443","kind":"mitm","allowed":true,"rule":"default","method":"GET","ua":"pip"}
`)
	out.Reset()
	require.NoError(t, SuggestEgress(dir, "", &out))
	assert.ContainsString(t, out.String(), "only GET and HEAD were seen")
	assert.ContainsString(t, out.String(), "\n  methods: [GET, HEAD]\n")

	_, err := os.Stat(dir)
	require.NoError(t, err)
	empty := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(empty, runsSubdir, "001-x"), 0o755))
	assert.Error(t, SuggestEgress(empty, "", &out), "no egress.log")
}

// leftovers go: a dead run's temp dirs past the age cutoff, and nothing
// younger.
func TestCleanTempDirs(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	root, err := sandbox.TempRoot()
	require.NoError(t, err)
	old, young := filepath.Join(root, "pats-work-old"), filepath.Join(root, "pats-work-young")
	require.NoError(t, os.MkdirAll(old, 0o700))
	require.NoError(t, os.MkdirAll(young, 0o700))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))

	var out bytes.Buffer
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, DryRun: true, Out: &out}))
	assert.Equal(t, out.String(), "INFO would remove temp dir "+old+"\n")
	_, err = os.Stat(old)
	require.NoError(t, err)

	// --json says the same as one object a line.
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, DryRun: true, JSON: true, Out: &out}))
	assert.Equal(t, out.String(), `{"action":"remove","dry_run":true,"place":"host","kind":"temp","name":"`+old+`"}`+"\n")

	// an old dir whose run is alive is kept: a run with no timeout can own
	// its dirs for days. the run's lease is fresh.
	live := filepath.Join(root, "pats-work-live")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(live+".owner", []byte("0123456789ab"), 0o600))
	lease := filepath.Join(filepath.Dir(root), "run-0123456789ab.lease")
	host, _ := os.Hostname()
	require.NoError(t, os.WriteFile(lease, fmt.Appendf(nil, "%s %d\n", host, os.Getpid()), 0o644))
	require.NoError(t, os.Chtimes(live, past, past))

	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, Out: &out}))
	_, err = os.Stat(old)
	assert.Error(t, err, assert.AnyError)
	_, err = os.Stat(young)
	require.NoError(t, err)
	_, err = os.Stat(live)
	require.NoError(t, err)

	// once its run is dead, the same dir goes, owner file and all.
	require.NoError(t, os.WriteFile(lease, fmt.Appendf(nil, "%s 999999999\n", host), 0o644))
	require.NoError(t, os.Chtimes(lease, past, past))
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, Out: &out}))
	_, err = os.Stat(live)
	assert.Error(t, err, assert.AnyError)
	_, err = os.Stat(live + ".owner")
	assert.Error(t, err, assert.AnyError)
}
