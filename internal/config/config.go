// Package config loads, validates, and expands a pats.yaml.
//
// the file is a mapping of vectors. the key is the vector's id; a vector has a
// `kind` (sandbox | agent | task | scorer | suite), optional `defaults` and
// non-empty `items` keyed by id. an item inherits every default it does not
// set; an explicit null unsets a field. a task vector may instead name a `dir`
// whose task.yaml files are its items. see load.go for the merge and
// pats.example.yaml for the shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
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
	Egress Egress `yaml:"egress"` // outbound network policy (absent: open)
	// Env goes into the sandboxed agent's environment, under what pats sets
	// itself. the escape hatch for a tool the egress tls contract misses
	// (e.g. DENO_CERT=/sbx-ca/bundle.pem).
	Env map[string]string `yaml:"env" tmpl:"-"`
}

// Egress is a sandbox's outbound network policy. absent, the network is
// open. the scalar `none` means no network at all. a mapping means the
// filtering proxy, configured by the embedded proxy.Policy (one field set for
// config and engine) plus the sidecar image override. see
// docs/proposals/egress-1x.md.
type Egress struct {
	None         bool `yaml:"-"` // `egress: none`
	Set          bool `yaml:"-"` // a mapping was given: the proxy runs
	proxy.Policy `yaml:",inline"`
	Image        string `yaml:"image"` // sidecar image override, docker only
}

// Mode names the policy's shape: open | none | proxy.
func (e Egress) Mode() string {
	switch {
	case e.None:
		return "none"
	case e.Set:
		return "proxy"
	}
	return "open"
}

// egressFields is Egress without its UnmarshalYAML, for decoding the mapping.
type egressFields Egress

// UnmarshalYAML accepts the scalar `none` or a mapping of policy fields. the
// loader's unknown-field check stops at the item's top level, so the mapping's
// keys are checked here.
func (e *Egress) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		if s != "none" {
			return fmt.Errorf("line %d: egress: want none or a mapping, got %q", node.Line, s)
		}
		*e = Egress{None: true}
		return nil
	case yaml.MappingNode:
		known := yamlKeys(reflect.TypeOf(egressFields{}))
		var errs errList
		for i := 0; i+1 < len(node.Content); i += 2 {
			if k := node.Content[i].Value; !known[k] {
				errs.add("line %d: egress: unknown field %q", node.Content[i].Line, k)
			}
		}
		if err := errs.err(); err != nil {
			return err
		}
		var f egressFields
		if err := node.Decode(&f); err != nil {
			return err
		}
		*e = Egress(f)
		e.Set = true
		return nil
	default:
		return fmt.Errorf("line %d: egress: want none or a mapping", node.Line)
	}
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

// Task is one scenario handed to a task-running agent. each run slot has an
// inline form (content) and a -cmd form (a command line); the prompt also has
// a -file form. Dir is where the task was defined, relative to the pats root:
// "" for pats.yaml, the task's directory for a task.yaml. its paths resolve
// and its scripts run there.
type Task struct {
	ID         string            `yaml:"-"`
	Vector     string            `yaml:"-"`
	Dir        string            `yaml:"-"`
	Prompt     string            `yaml:"prompt" tmpl:"-"`  // the instruction, literal
	PromptFile string            `yaml:"prompt-file"`      // a file holding the instruction
	PromptCmd  string            `yaml:"prompt-cmd"`       // a command printing the instruction; runs after prepare
	Prepare    string            `yaml:"prepare" tmpl:"-"` // inline script seeding the sandbox
	PrepareCmd string            `yaml:"prepare-cmd"`      // command seeding the sandbox
	Collect    string            `yaml:"collect" tmpl:"-"` // inline script gathering outputs
	CollectCmd string            `yaml:"collect-cmd"`      // command gathering outputs
	Timeout    string            `yaml:"timeout"`          // per-pair wall-clock limit (e.g. "15m"); "" -> 10m default; "0" -> none
	Env        map[string]string `yaml:"env" tmpl:"-"`     // exported to the task's host scripts
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

// Scorer scores one aspect of a task's collected output, 0.0 - 1.0. a script
// scorer sets `score` (inline) or `score-cmd`: exit 0 + first stdout line =
// float [0,1] or "na" (skip), exit != 0 = failure. an agent scorer sets
// `agent` plus one of the prompt keys instead: an agent judges the output.
type Scorer struct {
	ID         string `yaml:"-"`
	Vector     string `yaml:"-"`
	Dir        string `yaml:"-"`
	Score      string `yaml:"score" tmpl:"-"` // inline scoring script
	ScoreCmd   string `yaml:"score-cmd"`      // scoring command
	Agent      string `yaml:"agent"`          // agent scorer: the judging agent
	Prompt     string `yaml:"prompt" tmpl:"-"`
	PromptFile string `yaml:"prompt-file"`
	PromptCmd  string `yaml:"prompt-cmd"`
}

// IsAgent reports whether the scorer judges via an agent rather than a script.
func (s Scorer) IsAgent() bool { return s.Agent != "" }

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

// Load reads + parses a pats.yaml; `dir` vectors resolve against its
// directory. it does not Validate.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIn(filepath.Dir(path), data)
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
