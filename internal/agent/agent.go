// Package agent turns an agent definition into a sandbox execution. it knows
// the per-kind argv shape; the sandbox package handles isolation.
package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// one-shot harness invocations: model + prompt from PATS_* env, permissions
// bypassed for non-interactive use. each edits files in the cwd (/workspace),
// which the task's collect step then gathers; assistant text goes to stdout.
const (
	// claude-cli, keyless: model is an anthropic id; auth via oauth creds file
	// (~/.claude/.credentials.json), mounted into HOME by the run phase.
	// stream-json + verbose + partial messages make claude emit ndjson events as
	// they happen (plain --print buffers to exit), so the run-phase tee shows the
	// agent live. raw ndjson lands in stdout.log; scoring reads collected files,
	// not the stream, so the format change is harmless.
	// ${PATS_EFFORT:+...} adds --effort only when an effort is set (empty -> omitted).
	claudeKeylessCmd = `claude --print --output-format stream-json --verbose --include-partial-messages ${PATS_EFFORT:+--effort "$PATS_EFFORT"} --model "$PATS_MODEL" --permission-mode bypassPermissions "$(cat "$PATS_PROMPT_FILE")"`
	// opencode via openrouter: reads OPENROUTER_API_KEY from env. the openrouter/
	// prefix is added here so the config model stays e.g. openai/gpt-4o-mini.
	// --format json makes opencode emit one json event per line (tool_use,
	// step_finish with usage, text, reasoning) instead of bare assistant prose --
	// same role as claude's stream-json: a parseable stdout.log. --thinking
	// includes the reasoning events. effort maps to --variant (provider-specific
	// reasoning effort, e.g. high|max|minimal), same ${PATS_EFFORT:+...} trick.
	opencodeOpenrouterCmd = `opencode run --model "openrouter/$PATS_MODEL" --dangerously-skip-permissions --format json --thinking ${PATS_EFFORT:+--variant "$PATS_EFFORT"} "$(cat "$PATS_PROMPT_FILE")"`
)

// HarnessCmds maps an agent kind to its one-shot shell command -- the registry
// of supported kinds. (tests may temporarily override an entry to run a
// no-cred stand-in through the sandbox.)
var HarnessCmds = map[string]string{
	"claude-cli-keyless":  claudeKeylessCmd,
	"opencode-openrouter": opencodeOpenrouterCmd,
}

// RequiredHosts lists the hosts a harness itself must reach to function at all
// (inference api, auth/token refresh, startup fetches). the run phase merges
// these into a proxy-mode allowlist so pats.yaml only lists what the task
// needs. entries use the proxy's match syntax (".x.com" = x.com + subdomains).
var RequiredHosts = map[string][]string{
	"claude-cli-keyless": {
		".anthropic.com", // inference api
		".claude.com",    // oauth token refresh (missing it -> 401 mid-run)
	},
	"opencode-openrouter": {
		"openrouter.ai",      // inference
		"models.dev",         // opencode model catalog
		"registry.npmjs.org", // opencode runtime plugin/dep fetch
	},
}

// Spec builds the sandboxed execution for a task-running agent.
func Spec(a config.Agent, workdir string, env map[string]string) (sandbox.Spec, error) {
	cmd, ok := HarnessCmds[a.Kind]
	if !ok {
		return sandbox.Spec{}, fmt.Errorf("agent %q: unsupported kind %q", a.ID, a.Kind)
	}
	return sandbox.Spec{Argv: []string{"sh", "-c", cmd}, Workdir: workdir, Env: env}, nil
}

// Env assembles the PATS_* environment handed to a task-running agent.
// promptFile + outputDir are in-sandbox paths (see sandbox.WorkMount).
func Env(a config.Agent, promptFile, outputDir string) map[string]string {
	return map[string]string{
		"PATS_MODEL":       a.ResolvedModel(),
		"PATS_AGENT_KIND":  a.Kind,
		"PATS_PROMPT_FILE": promptFile,
		"PATS_OUTPUT_DIR":  outputDir,
		"PATS_EFFORT":      a.Effort,
	}
}

// credKeys are forwarded from the host env into a harness sandbox so the cli
// can authenticate. the agent runs as an arbitrary uid with no keychain access,
// so a token/key in the env is one way in (e.g. `claude setup-token` ->
// CLAUDE_CODE_OAUTH_TOKEN, or ANTHROPIC_API_KEY). the other is the creds file
// (see HostCredsFile), mounted mason-style.
//
// the list is per-kind: forwarding every key to every harness lets an unrelated
// credential sitting in the host env change a harness's auth mode behind your
// back, which for a keyless kind means it silently stops being keyless.
var credKeys = map[string][]string{
	"claude-cli-keyless": {
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
	},
	"opencode-openrouter": {"OPENROUTER_API_KEY"},
}

// CredEnv returns the cred-related env vars the given kind may inherit from the
// host, and whether any actual key/token (not just a base-url) was found.
func CredEnv(kind string) (env map[string]string, hasToken bool) {
	env = map[string]string{}
	for _, k := range credKeys[kind] {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
			if k != "ANTHROPIC_BASE_URL" {
				hasToken = true
			}
		}
	}
	return env, hasToken
}

// HostCredsFile returns the path to ~/.claude/.credentials.json if it exists,
// else "". this is claude's oauth creds (the "keyless" path); on linux it's a
// file, on macos it lives in the keychain so this returns "".
func HostCredsFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".claude", ".credentials.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}
