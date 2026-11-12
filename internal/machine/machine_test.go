package machine

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/images"
)

// the defaults are complete: every harness has a cli and a parse script, and
// every cli a version, which is the one the images are built with.
func TestDefaults(t *testing.T) {
	d := Defaults()
	assert.Equal(t, strings.Join(d.Names(), " "), "claude-cli-keyless codex-cli-keyless copilot-cli-keyless opencode-openrouter")
	for _, name := range d.Names() {
		h := d.Harness(name)
		assert.Equal(t, h.Name, name)
		require.That(t, h.Cli() != nil, "harness %s has no cli", name)
		assert.That(t, h.Parser() != nil, "harness %s has no parse script", name)
	}
	pins := images.ComponentPins()
	assert.Equal(t, len(d.CLIs), len(pins))
	for name, c := range d.CLIs {
		assert.Equal(t, c.Name, name)
		assert.Equal(t, c.Version, pins[name])
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
	const cli = "clis: {x: {version: 1.0.0, login-file: .x/login}}\n"
	for name, tc := range map[string]struct{ in, want string }{
		"unknown key":       {"harnesses: {h: {run: a, nope: 1}}", "field nope not found"},
		"unknown top key":   {"harness: {}", "field harness not found"},
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
  x: {version: 1.0.0, login-file: .x/login}
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
