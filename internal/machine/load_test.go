package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the machine's file is under $XDG_CONFIG_HOME when that is absolute, else
// under ~/.config, on every os.
func TestConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for xdg, want := range map[string]string{
		"":             filepath.Join(home, ".config", "pats", "config.yaml"),
		"relative/dir": filepath.Join(home, ".config", "pats", "config.yaml"),
		"/abs/config":  filepath.Join("/abs/config", "pats", "config.yaml"),
	} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got, err := ConfigPath()
		require.NoError(t, err)
		assert.Equal(t, got, want)
	}
}

// write puts a machine's file where Load looks, and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "pats", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

// the file is optional: with none pats runs on the defaults, and says
// nothing of it. one that is there and cannot be read is an error.
func TestLoadNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tb, err := Load()
	require.NoError(t, err)
	assert.Equal(t, tb.Found, false)
	assert.Equal(t, tb.Path, filepath.Join(dir, "pats", "config.yaml"))
	assert.Equal(t, tb.Note(), "")
	assert.Equal(t, strings.Join(tb.Names(), " "), strings.Join(Defaults().Names(), " "))

	// an empty file is a file: found, and it changes nothing.
	write(t, "")
	tb, err = Load()
	require.NoError(t, err)
	assert.Equal(t, tb.Found, true)
	assert.Equal(t, tb.Note(), "")
	assert.Equal(t, tb.Harness("codex-cli-keyless").From, FromDefault)

	// a directory where the file should be cannot be read as one.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pats", "config.yaml"), 0o755))
	t.Setenv("XDG_CONFIG_HOME", dir)
	_, err = Load()
	assert.Error(t, err, "machine config:")
}

// an entry named as a default changes it key by key: a key it sets is
// replaced whole, one it leaves out is kept, a null unsets one. an entry with
// a new name is added.
func TestOverlay(t *testing.T) {
	path := write(t, `
harnesses:
  claude-cli-keyless:
    egress:
      allow: [.anthropic.com]
    pass-env: ~
    login: false
  opencode-openrouter:
    parse: ~
    models: ~
  copilot-cli-keyless: {}
  mine:
    run: mine --go
    egress: {allow: [mine.example]}
    parse: |
      out.tool(line)
`)
	tb, err := Load()
	require.NoError(t, err)
	d := Defaults()

	claude := tb.Harness("claude-cli-keyless")
	assert.Equal(t, strings.Join(claude.Egress.Allow, " "), ".anthropic.com") // the list is restated, not added to
	assert.Equal(t, len(claude.PassEnv), 0)
	assert.Equal(t, claude.LoginFile(), "") // false over the default's true
	assert.Equal(t, claude.Run, d.Harness("claude-cli-keyless").Run)
	assert.Equal(t, strings.Join(claude.AuthEnv, " "), strings.Join(d.Harness("claude-cli-keyless").AuthEnv, " "))
	assert.That(t, claude.Parser() != nil, "a key left out lost its value")
	assert.Equal(t, claude.From, FromMachine)

	opencode := tb.Harness("opencode-openrouter")
	assert.That(t, opencode.Parser() == nil, "a null did not unset parse")
	assert.Equal(t, opencode.Models, "")

	mine := tb.Harness("mine")
	assert.Equal(t, mine.Run, "mine --go")
	assert.That(t, mine.Cli() == nil && mine.Parser() != nil, "the added entry")
	assert.Equal(t, mine.From, FromMachine)
	assert.Equal(t, tb.Harness("codex-cli-keyless").From, FromDefault)
	assert.Equal(t, tb.Harness("copilot-cli-keyless").From, FromDefault) // named, with nothing set

	assert.Equal(t, tb.Note(), "machine config "+path+": changes harnesses.claude-cli-keyless.egress, "+
		"harnesses.claude-cli-keyless.login, harnesses.claude-cli-keyless.pass-env, harnesses.opencode-openrouter.models, "+
		"harnesses.opencode-openrouter.parse; adds harnesses.mine")
}

// a file that is wrong is an error naming the file, the line and the entry,
// and nothing of it is applied.
func TestOverlayErrors(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"not yaml":         {"harnesses: [", "config.yaml:"},
		"not a mapping":    {"- a", "config.yaml:1: the top level is a mapping of tables"},
		"unknown table":    {"harnes: {}", `config.yaml:1: unknown table "harnes"`},
		"cli unknown key":  {"clis:\n  claude:\n    vers: 9.9.9", `config.yaml:3: clis.claude: unknown key "vers"`},
		"cli version only": {"clis:\n  claude:\n    version: 9.9.9", "config.yaml:2: clis.claude: version without artefacts"},
		"a harness's pin":  {"harnesses:\n  claude-cli-keyless:\n    version: 9.9.9", "config.yaml:3: harnesses.claude-cli-keyless: version is a pats.yaml's key: a machine pins a cli in clis"},
		"entries a list":   {"harnesses: [a]", "config.yaml:1: harnesses is a mapping of entries"},
		"entry a scalar":   {"harnesses:\n  mine: 5", "config.yaml:2: harnesses.mine is a mapping of keys"},
		"unknown key":      {"harnesses:\n  claude-cli-keyless:\n    hosts: [x.com]", `config.yaml:3: harnesses.claude-cli-keyless: unknown key "hosts"`},
		"new, no run":      {"\nharnesses:\n  mine:\n    cli: claude", "config.yaml:3: harnesses.mine: run is required"},
		"unknown cli":      {"harnesses:\n  claude-cli-keyless:\n    cli: nope", `config.yaml:2: harnesses.claude-cli-keyless: cli "nope" is not in clis`},
		"bad parse":        {"harnesses:\n  mine:\n    run: a\n    parse: out.tool(nope)", "config.yaml:2: harnesses.mine.parse:1:10: undefined: nope"},
		"a path in allow":  {"harnesses:\n  mine:\n    run: a\n    egress: {allow: [x.com/v1/*]}", "config.yaml:2: harnesses.mine: egress allow"},
		"wrong type":       {"harnesses:\n  mine:\n    run: a\n    auth-env: 5", "config.yaml:4: harnesses.mine: cannot unmarshal"},
		"nested key":       {"harnesses:\n  claude-cli-keyless:\n    egress:\n      alow: [x.com]", `config.yaml:4: harnesses.claude-cli-keyless: egress: unknown key "alow"`},
		"a key twice":      {"harnesses:\n  mine:\n    run: a\n    run: b", `config.yaml:4: harnesses.mine: key "run" is there twice`},
		"an entry twice":   {"harnesses:\n  mine: {run: a}\n  mine: {run: b}", "config.yaml:3: harnesses.mine is there twice"},
		"a table twice":    {"harnesses: {}\nharnesses: {}", `config.yaml:2: table "harnesses" is there twice`},
		"login, no file":   {"harnesses:\n  copilot-cli-keyless:\n    login: true", "config.yaml:2: harnesses.copilot-cli-keyless: login needs a cli with a login-file"},
	} {
		write(t, tc.in)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// an alias stands for what it names: an entry written once and used twice.
func TestOverlayAlias(t *testing.T) {
	write(t, "harnesses:\n  one: &base\n    run: base --go\n    egress: {allow: [base.example]}\n  two: *base\n")
	tb, err := Load()
	require.NoError(t, err)
	assert.Equal(t, tb.Harness("two").Run, "base --go")
	assert.Equal(t, tb.Harness("two").Digest(), tb.Harness("one").Digest())
	assert.Equal(t, tb.Harness("two").From, FromMachine)
}

// the digest is of the definition: it moves with a key of the entry and
// with its cli's facts, and not with the entry's name.
func TestDigest(t *testing.T) {
	a, b := Defaults(), Defaults()
	claude := a.Harness("claude-cli-keyless")
	assert.Equal(t, claude.Digest(), b.Harness("claude-cli-keyless").Digest())
	assert.Equal(t, len(claude.Digest()), 64)
	assert.That(t, claude.Digest() != a.Harness("codex-cli-keyless").Digest(), "two harnesses, one digest")

	was := claude.Digest()
	claude.Run += " --more"
	assert.That(t, claude.Digest() != was, "the digest did not move with run")
	claude.Run = b.Harness("claude-cli-keyless").Run
	claude.Cli().Version = "9.9.9"
	assert.That(t, claude.Digest() != was, "the digest did not move with the cli's version")

	// a copy under another name is the same definition.
	write(t, "harnesses:\n  copy:\n    run: \"true\"\n  other:\n    run: \"true\"\n")
	tb, err := Load()
	require.NoError(t, err)
	assert.Equal(t, tb.Harness("copy").Digest(), tb.Harness("other").Digest())
}

// a machine's file sets clis by the rule it sets harnesses by: a pin of a
// default cli changes it key by key, artefacts whole, and a new cli is one a
// harness of the file can name. a harness whose cli the file moved is from
// the machine, and its digest says its cli is another.
func TestOverlayCLIs(t *testing.T) {
	write(t, `
clis:
  codex:
    version: 9.9.9
    artefacts:
      https://mirror.example/codex-${version}.tgz:
        arch: [amd64, arm64]
        sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        bin: package/codex
  mine:
    version: 1.2.3
    artefacts:
      https://mine.example/mine-${version}:
        arch: [amd64]
        sha256: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
harnesses:
  mine-h:
    cli: mine
    run: mine "$PATS_PROMPT"
`)
	tb, err := Load()
	require.NoError(t, err)
	assert.ContainsString(t, tb.Note(), "changes clis.codex.artefacts, clis.codex.version")
	assert.ContainsString(t, tb.Note(), "adds clis.mine, harnesses.mine-h")

	codex := tb.CLIs["codex"]
	assert.Equal(t, codex.Version, "9.9.9")
	assert.Equal(t, codex.LoginFile, ".codex/auth.json") // a key the file did not name stays
	assert.Len(t, codex.Artefacts, 1)                    // artefacts are replaced whole
	url, a, ok := codex.ArtefactFor("arm64", "")
	require.That(t, ok, "no artefact for arm64")
	assert.Equal(t, url, "https://mirror.example/codex-9.9.9.tgz")
	assert.Equal(t, a.Bin, "package/codex")

	h := tb.Harness("codex-cli-keyless")
	assert.Equal(t, h.From, FromMachine)
	assert.Equal(t, tb.Harness("claude-cli-keyless").From, FromDefault) // a cli the file left alone
	assert.That(t, h.Digest() != Defaults().Harness("codex-cli-keyless").Digest(), "the digest did not move with the machine's pin")
	assert.Equal(t, tb.Harness("mine-h").Cli().Version, "1.2.3")
	_, _, ok = tb.CLIs["mine"].ArtefactFor("arm64", "")
	assert.That(t, !ok, "an artefact for an arch the entry does not name")
}

// pats config: the tables under a comment naming the file. with nothing
// changed they are the defaults as written, comments and all.
func TestPrint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tb, err := Load()
	require.NoError(t, err)
	var out strings.Builder
	require.NoError(t, tb.Print(&out))
	assert.Equal(t, out.String(), "# machine config: "+tb.Path+" (not there: these are the defaults)\n"+string(DefaultsYAML()))

	path := write(t, "harnesses:\n  noop:\n    run: \"true\"\n  codex-cli-keyless:\n    egress: {allow: [.openai.com]}\n")
	tb, err = Load()
	require.NoError(t, err)
	out.Reset()
	require.NoError(t, tb.Print(&out))
	assert.That(t, strings.HasPrefix(out.String(), "# machine config "+path+": changes harnesses.codex-cli-keyless.egress; adds harnesses.noop\n"), "%s", out.String())

	// what it prints reads back as the same tables.
	back, err := Parse([]byte(out.String()))
	require.NoError(t, err)
	assert.Equal(t, strings.Join(back.Names(), " "), strings.Join(tb.Names(), " "))
	for _, n := range tb.Names() {
		assert.Equal(t, back.Harness(n).Digest(), tb.Harness(n).Digest(), n)
	}
}
