// Package config loads, validates, and expands a pats.yaml.
//
// the file is a mapping of vectors. the key is the vector's id; a vector has a
// `kind` (sandbox | agent | task | scorer | suite), optional `defaults` and
// non-empty `items` keyed by id. an item inherits every default it does not
// set; an explicit null unsets a field. see load.go for the merge and
// pats.example.yaml for the shape.
package config

import (
	"os"
	"time"
)

// Config is a loaded pats.yaml: every item of every vector, merged with its
// vector's defaults and interpolated, in file order. Parse builds it; Validate
// checks it.
type Config struct {
	Sandboxes []Sandbox
	Agents    []Agent
	Tasks     []Task
	Scorers   []Scorer
	Suites    []Suite
}

// Sandbox is an isolation environment a task-running agent executes in.
// container drivers name their image one of two ways: `image` (pull/use
// as-is) or `build` (a docker build context pats builds at run start) -- at
// most one. neither means the published pats sandbox pinned to this version.
type Sandbox struct {
	ID     string `yaml:"-"`
	Vector string `yaml:"-"`
	Driver string `yaml:"driver"` // docker (default) | podman | bwrap
	Image  string `yaml:"image"`  // existing image to run; "" with no build -> the published pats sandbox
	Build  string `yaml:"build"`  // build context -- a dir (with a Dockerfile) or a Dockerfile path, relative to the config dir
	Egress Egress `yaml:"egress"` // outbound network policy (default: open)
}

// Egress is a sandbox's outbound network policy. see docs/proposals/network-egress.md.
//
//	open       -- open network (the default)
//	none       -- no network (--network none); only for agents needing no egress
//	proxy      -- filter through a sidecar proxy by host, allow/deny + audit
//	mitm-proxy -- proxy, plus url-level rules: hosts named in deny-urls or
//	              allow-urls get their tls terminated with a per-run CA (the
//	              agent trusts it via a merged bundle) so requests are filtered
//	              by full url. other hosts stay blind tunnels -- a superset of
//	              proxy.
type Egress struct {
	Mode    string   `yaml:"mode"`    // open | none | proxy | mitm-proxy
	Default string   `yaml:"default"` // proxy: deny (allowlist) | allow (denylist)
	Allow   []string `yaml:"allow"`   // hosts reachable when default: deny
	Deny    []string `yaml:"deny"`    // hosts blocked when default: allow
	// DenyURLs and AllowURLs (mitm-proxy only) are host-anchored url patterns;
	// `*` matches anything, `/` included. the host part must be a literal
	// hostname -- it picks which hosts get mitm'd. a deny match always loses
	// (e.g. "github.com/*/chisel-releases*"); a host with allow rules only
	// passes matching urls. hosts with no url rules are unaffected.
	DenyURLs  []string `yaml:"deny-urls"`
	AllowURLs []string `yaml:"allow-urls"`
	Image     string   `yaml:"proxy-image"` // override proxy image (default pats/egress-proxy:latest)
}

// IsContainer reports whether the sandbox runs under a container driver.
func (s Sandbox) IsContainer() bool { return s.Driver != "bwrap" }

// Agent is a harness (an agent cli) under test, run in a sandbox. four
// harnesses:
//
//	opencode-openrouter -- opencode cli, models via openrouter (OPENROUTER_API_KEY)
//	claude-cli-keyless  -- claude cli, keyless oauth creds (~/.claude/.credentials.json)
//	codex-cli-keyless   -- codex cli, keyless chatgpt creds (~/.codex/auth.json)
//	copilot-cli-keyless -- copilot cli, keyless github token (COPILOT_GITHUB_TOKEN)
type Agent struct {
	ID      string `yaml:"-"`
	Vector  string `yaml:"-"`
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Sandbox string `yaml:"sandbox"` // a sandbox id; omitted -> the only sandbox defined
	Effort  string `yaml:"effort"`  // optional reasoning effort (claude/copilot --effort; codex model_reasoning_effort; opencode --variant)
}

// Task is one scenario handed to a task-running agent.
type Task struct {
	ID      string `yaml:"-"`
	Vector  string `yaml:"-"`
	Prompt  string `yaml:"prompt"`  // the instruction: exec a file, read a file, or the literal text (see resolvePrompt)
	Prepare string `yaml:"prepare"` // seed the sandbox (optional)
	Collect string `yaml:"collect"` // gather outputs (optional)
	Timeout string `yaml:"timeout"` // per-pair wall-clock limit (e.g. "15m"); "" -> 10m default; "0" -> none
}

// DefaultTimeout bounds an agent run when a task sets no timeout.
const DefaultTimeout = 10 * time.Minute

// TimeoutDuration is the task's per-pair wall-clock limit: its own timeout, the
// 10m default when unset, or 0 (no limit) when explicitly "0". assumes the string
// validated (Validate parses it).
func (t Task) TimeoutDuration() time.Duration {
	if t.Timeout == "" {
		return DefaultTimeout
	}
	d, _ := time.ParseDuration(t.Timeout)
	return d
}

// Scorer scores one aspect of a task's collected output, 0.0 - 1.0. a file
// scorer sets `score`: pats execs it directly, its shebang picks the
// interpreter + deps, exit 0 + first stdout line = float [0,1] or "na" (skip),
// exit != 0 = failure. an agent scorer sets `agent-id` + `prompt` instead:
// an agent judges the output.
type Scorer struct {
	ID      string `yaml:"-"`
	Vector  string `yaml:"-"`
	Score   string `yaml:"score"`    // file scorer: the script to exec
	AgentID string `yaml:"agent-id"` // agent scorer: the judging agent
	Prompt  string `yaml:"prompt"`   // agent scorer: the judging instruction (see resolvePrompt)
}

// IsAgent reports whether the scorer judges via an agent rather than a script.
func (s Scorer) IsAgent() bool { return s.AgentID != "" }

// Suite is a named (agents, tasks, scorers) selection. both cross-products are
// implied within it: run = agents x tasks, score = tasks x scorers. each axis
// lists vector or item ids of that kind; a vector id expands to its items.
// scorers may be empty (a run-only suite); agents and tasks may not.
type Suite struct {
	ID      string  `yaml:"-"`
	Vector  string  `yaml:"-"`
	Agents  StrList `yaml:"agents"`
	Tasks   StrList `yaml:"tasks"`
	Scorers StrList `yaml:"scorers"`
}

// StrList accepts a yaml scalar or sequence and stores it as []string.
type StrList []string

// Load reads + parses a pats.yaml. it does not Validate.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Kinds lists the vector kinds, in the order the resolved config holds them.
var Kinds = []string{"sandbox", "agent", "task", "scorer", "suite"}

// Harnesses is the set of supported agent harnesses. the harness->command
// mapping lives in agent.HarnessCmds; config can't import it (would cycle), so
// the names are repeated here -- a test in the agent package keeps the two in
// sync.
var Harnesses = map[string]bool{
	"opencode-openrouter": true,
	"claude-cli-keyless":  true,
	"codex-cli-keyless":   true,
	"copilot-cli-keyless": true,
}

// harnessList names the harnesses in error messages.
const harnessList = "opencode-openrouter|claude-cli-keyless|codex-cli-keyless|copilot-cli-keyless"
