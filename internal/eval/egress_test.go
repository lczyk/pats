package eval

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

const egressCfg = `box: {kind: sandbox, items: {s: {image: img, egress: {allow: [a.test, '.ubuntu.com'], deny: ['a.test/secret*']}}, open: {image: img}}}
a: {kind: agent, items: {a: {harness: claude-cli-keyless, model: m, sandbox: s}}}
t: {kind: task, items: {t: {prompt: p, egress: {allow: [pypi.org], deny: ['github.com/*/answer*']}}}}
su: {kind: suite, items: {su: {agents: a, tasks: t}}}
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
	pol, err := EffectivePolicy(c, "s", "", "")
	require.NoError(t, err)
	assert.EqualArrays(t, pol.Allow, []string{"a.test", ".ubuntu.com"})

	pol, err = EffectivePolicy(c, "s", "t", "a")
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(pol.Allow, " "), "pypi.org")
	assert.ContainsString(t, strings.Join(pol.Allow, " "), ".anthropic.com")
	assert.EqualArrays(t, pol.Deny, []string{"a.test/secret*", "github.com/*/answer*"})

	for _, bad := range [][3]string{{"ghost", "", ""}, {"open", "", ""}, {"s", "ghost", ""}, {"s", "", "ghost"}} {
		_, err := EffectivePolicy(c, bad[0], bad[1], bad[2])
		assert.Error(t, err, assert.AnyError, bad[0]+bad[1]+bad[2])
	}
}

func TestCheckURL(t *testing.T) {
	c := egressConfig(t)
	pol, err := EffectivePolicy(c, "s", "t", "a")
	require.NoError(t, err)
	cases := []struct {
		url, want string
		ok        bool
	}{
		{"https://a.test/x", "allow rule=a.test  (mitm;", true},
		{"https://a.test/secret/1", "deny  rule=a.test/secret*  (mitm;", false},
		{"archive.ubuntu.com", "allow rule=.ubuntu.com  (tunnel;", true},
		{"http://archive.ubuntu.com/x.deb", "allow rule=.ubuntu.com  (plain http;", true},
		{"https://evil.test/", "deny  rule=default  (tunnel;", false},
		{"https://github.com/canonical/answer", "deny  rule=default", false}, // github is not allowed at all
		{"https://api.anthropic.com/v1", "allow rule=.anthropic.com", true},
		{"https://a.test:8443/", "deny  rule=default", false},
		{"https://a.test/%73ecret/1", "deny  rule=a.test/secret*", false}, // decoded, as the proxy sees it
		{"https://a.test", "allow rule=a.test  (mitm;", true},             // empty path is "/"
	}
	for _, tc := range cases {
		var out bytes.Buffer
		ok, err := CheckURL(pol, tc.url, "GET", true, &out)
		require.NoError(t, err)
		assert.Equal(t, ok, tc.ok, tc.url)
		assert.ContainsString(t, out.String(), tc.want)
		assert.Equal(t, strings.Contains(out.String(), "harness hosts not merged"), false)
	}

	pol, _ = EffectivePolicy(c, "s", "", "")
	var out bytes.Buffer
	ok, err := CheckURL(pol, "https://api.anthropic.com/", "", false, &out)
	require.NoError(t, err)
	assert.Equal(t, ok, false)
	assert.ContainsString(t, out.String(), "harness hosts not merged")

	_, err = CheckURL(pol, "https://", "GET", false, &out)
	assert.Error(t, err, "bad url")
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
	require.NoError(t, os.MkdirAll(filepath.Join(run, "a", "t"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(run, "a", "t", "egress.log"), []byte(audit), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(run, "a", "t", "metadata.json"), []byte(`{"agent":"a","harness":"claude-cli-keyless"}`), 0o644))
	return dir
}

func TestSuggestEgress(t *testing.T) {
	dir := writeRun(t, sampleAudit)
	var out bytes.Buffer
	require.NoError(t, SuggestEgress(dir, "", &out))
	s := out.String()
	for _, want := range []string{
		"egress:\n  allow:\n",
		"# tunnels (no user agent or method visible)\n    - archive.ubuntu.com\n    - evil.test\n    - 'git.test:22'\n",
		"# curl/8.5.0\n    - gist.github.com\n",
		"# pip/24.0\n    - pypi.org\n",
		"# methods: unknown -- tunnels hide them",
		"# harness hosts, merged per agent by pats: api.anthropic.com",
	} {
		assert.ContainsString(t, s, want)
	}
	assert.Equal(t, strings.Contains(s, "github.com/c/answer"), false) // an explicit deny is not suggested

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

// leftovers go: temp dirs past the age cutoff, and nothing younger.
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
	assert.ContainsString(t, out.String(), "would remove "+old)
	_, err = os.Stat(old)
	require.NoError(t, err)

	// an old dir whose maker still runs is kept: a run with no timeout can
	// own its dirs for days.
	live, err := sandbox.MkTemp("pats-work-live-")
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(live, past, past))

	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, Out: &out}))
	_, err = os.Stat(old)
	assert.Error(t, err, assert.AnyError)
	_, err = os.Stat(young)
	require.NoError(t, err)
	_, err = os.Stat(live)
	require.NoError(t, err)

	// once its maker is gone, the same dir goes, owner file and all.
	require.NoError(t, os.WriteFile(live+".owner", []byte("999999999"), 0o600))
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{}, OlderThan: 24 * time.Hour, Out: &out}))
	_, err = os.Stat(live)
	assert.Error(t, err, assert.AnyError)
	_, err = os.Stat(live + ".owner")
	assert.Error(t, err, assert.AnyError)
}

// an agent, sidecar and network belong to a live pair only while the pats
// process that made the pair's workdir runs; a crashed run's go in the same
// pass, the agent before the network it holds. what another owner made is
// left unless forced: its workdir is not ours to look at.
func TestCleanContainersByOwner(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	live, err := sandbox.MkTemp("pats-work-")
	require.NoError(t, err)
	dead, err := sandbox.MkTemp("pats-work-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dead+".owner", []byte("999999999"), 0o600))

	// a fake driver lists, per prefix, our live and dead pair's names, plus a
	// foreign one when not filtered by owner label; it logs every call.
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
case "$1 $2" in
  "ps -a"|"network ls")
    p=${4#name=^}
    printf '%s%s\n%s%s\n' $p ` + filepath.Base(live) + ` $p ` + filepath.Base(dead) + `
    case "$*" in *label=pats.owner=*) ;; *) printf '%sforeign\n' $p ;; esac ;;
esac
`
	drv := filepath.Join(bin, "fakedriver")
	require.NoError(t, os.WriteFile(drv, []byte(script), 0o755))

	var out bytes.Buffer
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{drv}, OlderThan: 24 * time.Hour, Out: &out}))
	b, err := os.ReadFile(calls)
	require.NoError(t, err)
	log := string(b)
	assert.ContainsString(t, log, "label="+sandbox.OwnerLabel())
	assert.ContainsString(t, log, "rm -f sbx-proxy-"+filepath.Base(dead))
	assert.ContainsString(t, log, "network rm sbx-egr-"+filepath.Base(dead))
	agentRm := strings.Index(log, "rm -f sbx-agent-"+filepath.Base(dead))
	require.That(t, agentRm >= 0, "dead agent not removed:\n%s", log)
	assert.That(t, agentRm < strings.Index(log, "network rm sbx-egr-"+filepath.Base(dead)))
	for _, n := range []string{"sbx-agent-", "sbx-proxy-", "sbx-egr-"} {
		assert.Equal(t, strings.Contains(log, " "+n+filepath.Base(live)+"\n"), false, "live %s removed", n)
		assert.Equal(t, strings.Contains(log, n+"foreign"), false, "foreign %s removed unforced", n)
	}
	assert.ContainsString(t, out.String(), "left 3 not made by")

	require.NoError(t, os.Remove(calls))
	out.Reset()
	require.NoError(t, Clean(context.Background(), CleanOptions{Drivers: []string{drv}, OlderThan: 24 * time.Hour, Force: true, Out: &out}))
	b, err = os.ReadFile(calls)
	require.NoError(t, err)
	log = string(b)
	assert.ContainsString(t, log, "rm -f sbx-proxy-foreign")
	assert.ContainsString(t, log, "network rm sbx-egr-foreign")
	assert.Equal(t, strings.Contains(log, " sbx-proxy-"+filepath.Base(live)+"\n"), false, "forced clean removed a live pair")
}
