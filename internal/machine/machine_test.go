package machine

import (
	"regexp"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"gopkg.in/yaml.v3"
)

// the defaults are complete: every harness has a cli and a parse script, and
// every cli an exact version, which is its pin.
func TestDefaults(t *testing.T) {
	d := Defaults()
	assert.Equal(t, strings.Join(d.Names(), " "), "claude-cli-keyless codex-cli-keyless copilot-cli-keyless opencode-openrouter pi-openrouter")
	for _, name := range d.Names() {
		h := d.Harness(name)
		assert.Equal(t, h.Name, name)
		require.That(t, h.Cli() != nil, "harness %s has no cli", name)
		assert.That(t, h.Parser() != nil, "harness %s has no parse script", name)
	}
	exact := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	assert.Equal(t, strings.Join(sortedKeys(d.CLIs), " "), "claude codex copilot opencode pi")
	for name, c := range d.CLIs {
		assert.Equal(t, c.Name, name)
		assert.That(t, exact.MatchString(c.Version), "cli %s: %q is not an exact version", name, c.Version)
		// an artefact for each arch, at the cli's version.
		for _, arch := range Arches {
			url, a, ok := c.ArtefactFor(arch, "")
			require.That(t, ok, "cli %s has no artefact for %s", name, arch)
			assert.ContainsString(t, url, c.Version)
			assert.That(t, !strings.Contains(url, "${"), "%s: the url still holds a placeholder: %s", name, url)
			assert.That(t, a.Bin != "", "cli %s: no bin for %s", name, arch)
		}
	}
}

// the commands are folded yaml scalars: pin them to the exact strings a
// shell gets, since a stray newline or a lost space changes what runs.
func TestDefaultCommands(t *testing.T) {
	d := Defaults()
	for name, want := range map[string]string{
		"claude-cli-keyless":  `claude --print --output-format stream-json --verbose --include-partial-messages ${PATS_EFFORT:+--effort "$PATS_EFFORT"} --model "$PATS_MODEL" --permission-mode bypassPermissions "$(cat "$PATS_PROMPT_FILE")"`,
		"opencode-openrouter": `opencode run --model "openrouter/$PATS_MODEL" --dangerously-skip-permissions --format json --thinking ${PATS_EFFORT:+--variant "$PATS_EFFORT"} "$(cat "$PATS_PROMPT_FILE")"`,
		"codex-cli-keyless":   `codex exec --json --ephemeral --ignore-user-config --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox --model "$PATS_MODEL" ${PATS_EFFORT:+-c model_reasoning_effort="$PATS_EFFORT"} - < "$PATS_PROMPT_FILE"`,
		"copilot-cli-keyless": `copilot -p "$(cat "$PATS_PROMPT_FILE")" --allow-all --output-format json --no-auto-update --no-remote-export --disable-builtin-mcps --model "$PATS_MODEL" ${PATS_EFFORT:+--effort "$PATS_EFFORT"}`,
		"pi-openrouter":       `PI_OFFLINE=1 pi --print --mode json --no-session --approve --provider openrouter --model "$PATS_MODEL" ${PATS_EFFORT:+--thinking "$PATS_EFFORT"} -- "$(cat "$PATS_PROMPT_FILE")"`,
	} {
		assert.Equal(t, d.Harness(name).Run, want)
	}
}

// what each default knows of logging in and of its hosts.
func TestDefaultEntries(t *testing.T) {
	d := Defaults()
	claude, codex, copilot, opencode := d.Harness("claude-cli-keyless"), d.Harness("codex-cli-keyless"), d.Harness("copilot-cli-keyless"), d.Harness("opencode-openrouter")

	assert.Equal(t, claude.LoginFile(), ".claude/.credentials.json")
	assert.Equal(t, claude.Cli().AuthPathVar(), "PATS_CLAUDE_AUTH_PATH")
	assert.Equal(t, strings.Join(claude.PassEnv, " "), "ANTHROPIC_BASE_URL")
	assert.Equal(t, codex.LoginFile(), ".codex/auth.json")
	assert.Equal(t, len(codex.AuthEnv), 0) // what keeps it keyless
	assert.Equal(t, codex.Cli().HomeEnv["CODEX_HOME"], ".codex")
	assert.Equal(t, copilot.LoginFile(), "")
	assert.Equal(t, opencode.Models, "openrouter")

	assert.Equal(t, strings.Join(claude.Egress.Allow, " "), ".anthropic.com .claude.com")
	assert.Equal(t, strings.Join(codex.Egress.Allow, " "), ".openai.com .chatgpt.com")
	assert.Equal(t, strings.Join(copilot.Egress.Allow, " "), ".github.com .githubcopilot.com")
	assert.Equal(t, strings.Join(opencode.Egress.Allow, " "), "openrouter.ai models.dev models.opencode.ai registry.npmjs.org")

	var none *Harness
	assert.That(t, none.Parser() == nil, "no entry, no parser")
}

// each call of Defaults is the caller's own copy.
func TestDefaultsAreCopies(t *testing.T) {
	a, b := Defaults(), Defaults()
	a.Harness("codex-cli-keyless").Run = "true"
	assert.That(t, b.Harness("codex-cli-keyless").Run != "true", "one copy's edit reached another")
}

func TestParseErrors(t *testing.T) {
	zeros := strings.Repeat("0", 64)
	const cli = "clis: {x: {version: 1.0.0, login-file: .x/login}}\n"
	for name, tc := range map[string]struct{ in, want string }{
		"unknown key":       {"harnesses: {h: {run: a, nope: 1}}", `line 1: harnesses.h: unknown key "nope"`},
		"unknown top key":   {"harness: {}", `line 1: unknown table "harness"`},
		"no run":            {"harnesses: {h: {cli: x}}\n" + cli, "harnesses.h: run is required"},
		"bad harness name":  {"harnesses: {H_1: {run: a}}", "harnesses.H_1: a name is"},
		"bad cli name":      {"clis: {X: {version: 1.0.0}}", "clis.X: a name is"},
		"no version":        {"clis: {x: {}}", "clis.x: version is required"},
		"unknown cli":       {"harnesses: {h: {run: a, cli: y}}", `harnesses.h: cli "y" is not in clis`},
		"login, no cli":     {"harnesses: {h: {run: a, login: true}}", "harnesses.h: login needs a cli with a login-file"},
		"login, no file":    {"harnesses: {h: {run: a, cli: x, login: true}}\nclis: {x: {version: 1.0.0}}", "harnesses.h: login needs a cli with a login-file"},
		"unknown catalogue": {"harnesses: {h: {run: a, models: nope}}", `harnesses.h: models "nope" is not a catalogue`},
		"parse syntax":      {"harnesses: {h: {run: a, parse: 'out.tool('}}", "harnesses.h.parse:"},
		"parse names":       {"harnesses: {h: {run: a, parse: 'out.tool(nope)'}}", "harnesses.h.parse:1:10: undefined: nope"},
		"path in allow":     {"harnesses: {h: {run: a, egress: {allow: [x.com/v1/*]}}}", "a harness takes hosts only"},
		"method in allow":   {"harnesses: {h: {run: a, egress: {allow: ['GET x.com/**']}}}", "a harness takes hosts only"},
		"regexp in deny":    {"harnesses: {h: {run: a, egress: {deny: ['x.com ~^/v1']}}}", "a harness takes hosts only"},
		"bad entry":         {"harnesses: {h: {run: a, egress: {allow: ['']}}}", "harnesses.h: egress allow"},
		"env not a name":    {"harnesses: {h: {run: a, auth-env: [A-B]}}", "harnesses.h: auth-env A-B is not a variable's name"},
		"env is pats's":     {"harnesses: {h: {run: a, pass-env: [PATS_MODEL]}}", "harnesses.h: pass-env PATS_MODEL is pats's own to set"},
		"env is home":       {"clis: {x: {version: 1.0.0, home-env: {HOME: .x}}}", "clis.x: home-env HOME is pats's own to set"},
		"env twice":         {"harnesses: {h: {run: a, auth-env: [K], pass-env: [K]}}", "harnesses.h: pass-env K is also in auth-env"},
		"env in home-env":   {"harnesses: {h: {run: a, cli: x, auth-env: [X_HOME]}}\nclis: {x: {version: 1.0.0, home-env: {X_HOME: .x}}}", "harnesses.h: auth-env X_HOME is also in its cli's home-env"},
		"artefact url":      {"clis: {x: {version: 1.0.0, artefacts: {'ftp://x/y': {arch: [amd64], sha256: " + zeros + "}}}}", "is not an http or https url"},
		"no artefacts":      {"clis: {x: {version: 1.0.0}}", "clis.x: at least one artefact is required"},
		"url, no version":   {"clis: {x: {version: 1.0.0, artefacts: {'https://x/latest.tgz': {arch: [amd64], sha256: " + zeros + "}}}}", "the url has no ${version}"},
		"artefact arch":     {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {arch: [riscv64], sha256: " + zeros + "}}}}", `arch "riscv64" is not one of amd64, arm64`},
		"artefact no arch":  {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {sha256: " + zeros + "}}}}", "arch is required"},
		"artefact sha":      {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {arch: [amd64], sha256: abc}}}}", "sha256 is 64 lower-case hex digits"},
		"artefact bin":      {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {arch: [amd64], sha256: " + zeros + ", bin: ../x}}}}", "is not a path inside the artefact"},
		"artefact key":      {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {arch: [amd64], sha256: " + zeros + ", exe: x}}}}", `artefacts https://x/y: unknown key "exe"`},
		"two for one arch":  {"clis: {x: {version: 1.0.0, artefacts: {'https://x/y': {arch: [amd64], sha256: " + zeros + "}, 'https://x/z': {arch: [amd64, arm64], sha256: " + zeros + "}}}}", "two artefacts are for amd64"},
		"login-file leaves": {"clis: {x: {version: 1.0.0, login-file: ../.ssh/id}}", "clis.x: login-file"},
		"login-file abs":    {"clis: {x: {version: 1.0.0, login-file: /etc/passwd}}", "clis.x: login-file"},
		"home-env leaves":   {"clis: {x: {version: 1.0.0, home-env: {X_HOME: ../x}}}", "clis.x: home-env X_HOME"},
	} {
		_, err := Parse([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// what is allowed: a harness with no cli, hosts with a suffix and a port, a
// harness that logs in from its cli's file.
func TestParse(t *testing.T) {
	tb, err := Parse([]byte(`
clis:
  x:
    version: 1.0.0
    login-file: .x/login
    artefacts:
      https://x.example/x-${version}.tgz: {arch: [amd64, arm64], sha256: "` + strings.Repeat("0", 64) + `"}
harnesses:
  bare: {run: "true"}
  full:
    cli: x
    run: x go
    login: true
    egress: {allow: [.x.com, "api.x.com:8443"], deny: [.noise.x.com]}
`))
	require.NoError(t, err)
	assert.That(t, tb.Harness("bare").Cli() == nil, "a harness with no cli got one")
	assert.That(t, tb.Harness("bare").Parser() == nil, "a harness with no parse got a parser")
	assert.Equal(t, tb.Harness("bare").LoginFile(), "")
	assert.Equal(t, tb.Harness("full").LoginFile(), ".x/login")
	assert.Equal(t, tb.Harness("full").Cli().AuthPathVar(), "PATS_X_AUTH_PATH")
	assert.That(t, tb.Harness("nope") == nil, "an unknown harness is nil")
}

// a project's items are laid over the tables as the machine's file is: over
// the harness of their name key by key, a null removing a key, or as a new
// harness. the result is checked as a whole.
func TestProject(t *testing.T) {
	item := func(name, body string) Item {
		var doc yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte(body), &doc))
		return Item{Name: name, Node: doc.Content[0]}
	}
	tb := Defaults()
	require.NoError(t, tb.Project([]Item{
		item("claude-cli-keyless", "{pass-env: ~, egress: {allow: [.anthropic.com]}}"),
		item("noop", `{run: "true", parse: ~}`),
	}))
	claude := tb.Harness("claude-cli-keyless")
	assert.Equal(t, len(claude.PassEnv), 0)
	assert.Equal(t, strings.Join(claude.Egress.Allow, " "), ".anthropic.com")
	assert.Equal(t, claude.Run, Defaults().Harness("claude-cli-keyless").Run)
	assert.Equal(t, claude.LoginFile(), ".claude/.credentials.json") // a key left out keeps the machine's
	assert.Equal(t, claude.From, FromProject)
	assert.Equal(t, tb.Harness("noop").Run, "true")
	assert.Equal(t, tb.Harness("noop").From, FromProject)
	assert.Equal(t, tb.Harness("codex-cli-keyless").From, FromDefault)

	for name, tc := range map[string]struct {
		it   Item
		want string
	}{
		"new, no run": {item("mine", "{cli: claude}"), "harnesses.mine: run is required"},
		"unknown cli": {item("mine", "{run: a, cli: nope}"), `harnesses.mine: cli "nope" is not in clis`},
		"bad name":    {item("Mine", "{run: a}"), "harnesses.Mine: a name is"},
		"bad parse":   {item("mine", "{run: a, parse: 'out.tool('}"), "harnesses.mine.parse:"},
		"wrong type":  {item("mine", "{run: a, auth-env: 5}"), "line 1: harnesses.mine: cannot unmarshal"},
		"nested key":  {item("mine", "{run: a, egress: {alow: [x.com]}}"), `line 1: harnesses.mine: egress: unknown key "alow"`},
	} {
		err := Defaults().Project([]Item{tc.it})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
	require.NoError(t, Defaults().Project(nil))
}

// a cli's artefact for an arch, its url at the cli's version or another, and
// the digest moving with what a tree holds and not with where it is from.
func TestArtefacts(t *testing.T) {
	zeros, ones := strings.Repeat("0", 64), strings.Repeat("1", 64)
	src := func(host, sha, bin string) string {
		return "clis:\n  x:\n    version: 1.0.0\n    artefacts:\n      https://" + host + "/x-${version}-x64.tgz:\n        arch: [amd64]\n        sha256: " + sha + "\n        bin: " + bin + "\nharnesses: {h: {run: a, cli: x}}\n"
	}
	tb, err := Parse([]byte(src("a.example", zeros, "package/x")))
	require.NoError(t, err)
	c := tb.CLIs["x"]
	url, a, ok := c.ArtefactFor("amd64", "")
	require.That(t, ok, "no artefact for amd64")
	assert.Equal(t, url, "https://a.example/x-1.0.0-x64.tgz")
	assert.Equal(t, a.Bin, "package/x")
	url, _, _ = c.ArtefactFor("amd64", "2.0.0")
	assert.Equal(t, url, "https://a.example/x-2.0.0-x64.tgz")
	key, _, _ := c.ArtefactKey("amd64")
	assert.Equal(t, key, "https://a.example/x-${version}-x64.tgz")
	_, _, ok = c.ArtefactFor("arm64", "")
	assert.Equal(t, ok, false)

	digest := func(host, sha, bin string) string {
		tb, err := Parse([]byte(src(host, sha, bin)))
		require.NoError(t, err)
		return tb.Harness("h").Digest()
	}
	base := digest("a.example", zeros, "package/x")
	assert.Equal(t, digest("mirror.example", zeros, "package/x"), base) // a url is where, not what
	assert.That(t, digest("a.example", ones, "package/x") != base, "the digest did not move with the sha")
	assert.That(t, digest("a.example", zeros, "package/bin/x") != base, "the digest did not move with the bin")
}
