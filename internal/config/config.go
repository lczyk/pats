// Package config loads, validates, and expands a pats.yaml.
//
// the file is a mapping of vectors. the key is the vector's name; a vector has a
// `kind` (sandbox | agent | task | scorer | suite), optional `defaults` and
// non-empty `items` keyed by name. an item inherits every default it does not
// set; an explicit null unsets a field. a task vector may instead name a `dir`
// whose task.yaml files are its items. see load.go for the merge and
// pats.example.yaml for the shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
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
	// Harnesses are the harness and cli tables the config runs with: this
	// config's own copy, so nothing done to it reaches another.
	Harnesses *machine.Tables
}

// builtins answers for a Config that was built by hand and carries no tables.
var builtins = sync.OnceValue(machine.Defaults)

// HarnessOf is the entry an agent's harness names, nil when there is none.
func (c *Config) HarnessOf(a Agent) *machine.Harness {
	if c == nil || c.Harnesses == nil {
		return builtins().Harness(a.Harness)
	}
	return c.Harnesses.Harness(a.Harness)
}

// Sandbox is an isolation environment a task-running agent executes in.
// container drivers name their image one of two ways: `image` (pull/use
// as-is) or `build` (a docker build context pats builds at run start) -- at
// most one. neither means the published pats sandbox pinned to this version.
type Sandbox struct {
	Name   string `yaml:"-"`
	Vector string `yaml:"-"`
	Driver string `yaml:"driver"` // docker (default) | podman | lxd | bwrap | host
	// SandNotBoxed is the host driver's acknowledgement: the agent runs on
	// this machine, as the user, with no isolation. host only, and required.
	SandNotBoxed bool   `yaml:"sand-not-boxed"`
	Image        string `yaml:"image"` // existing image to run, in the driver's own namespace; "" -> built or the pats sandbox
	Build        string `yaml:"build"` // build context -- a dir (with a Dockerfile) or a Dockerfile path, relative to the config dir
	// Base and Provision are the driver-neutral way to make an image: the
	// provision script runs once per image, as root, on top of base (the pats
	// sandbox when base is empty). docker compiles them to a dockerfile, lxd
	// provisions a builder instance.
	Base        string `yaml:"base"`
	Provision   string `yaml:"provision"`
	Sudo        bool   `yaml:"sudo"`         // may the agent sudo; a task can override (lxd only)
	MaxParallel int    `yaml:"max-parallel"` // cap on this sandbox's concurrent pairs, whatever -j says
	Limits      Limits `yaml:"limits"`
	// lxd only.
	Remote   string            `yaml:"remote"` // an lxc remote; "" -> the cli's current one
	VM       bool              `yaml:"vm"`     // a virtual machine rather than a system container
	Profiles []string          `yaml:"profiles"`
	Config   map[string]string `yaml:"config"`
	Egress   Egress            `yaml:"egress"` // outbound network policy (absent: open)
	// Env goes into the sandboxed agent's environment, under what pats sets
	// itself. the escape hatch for a tool the egress tls contract misses
	// (e.g. DENO_CERT=/sbx-ca/bundle.pem).
	Env map[string]string `yaml:"env"`
	// ProxyImage is the egress proxy image a run starts its proxies from: the
	// pinned one, set by the run from the pins, never by pats.yaml.
	ProxyImage string `yaml:"-"`
	// Components are the harness versions stamped into the pinned image.
	Components map[string]string `yaml:"-"`
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
func (s Sandbox) IsContainer() bool { return sandbox.IsEngine(s.Driver) }

// Limits caps a sandbox's resources. docker maps cpu and memory to --cpus and
// --memory; lxd maps all three to limits.cpu, limits.memory and the root
// disk's size.
type Limits struct {
	CPU    int    `yaml:"cpu"`
	Memory string `yaml:"memory"` // e.g. 8GiB
	Disk   string `yaml:"disk"`   // e.g. 20GiB
}

// Agent is a harness (an agent cli) under test, run in a sandbox. Harness
// names an entry of the config's harness table (Config.Harnesses).
type Agent struct {
	Name    string `yaml:"-"`
	Vector  string `yaml:"-"`
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Sandbox string `yaml:"sandbox"` // a sandbox name; omitted -> the only sandbox defined
	Effort  string `yaml:"effort"`  // optional reasoning effort (claude/copilot --effort; codex model_reasoning_effort; opencode --variant)
	// MaxCost stops a pair once its reported spend passes it, in us dollars;
	// nil is no cap. only a harness that reports cost as it runs takes one.
	MaxCost *float64 `yaml:"max-cost"`
}

// Task is one scenario handed to a task-running agent. each step is a shell
// script; the prompt is text, a file or a script's stdout. Dir is where the
// task was defined, relative to the pats root:
// "" for pats.yaml, the task's directory for a task.yaml. its paths resolve
// and its scripts run there.
type Task struct {
	Name       string            `yaml:"-"`
	Vector     string            `yaml:"-"`
	Dir        string            `yaml:"-"`
	Prompt     string            `yaml:"prompt" text:"free"` // the instruction, literal
	PromptFile string            `yaml:"prompt-file"`        // a file holding the instruction
	PromptCmd  string            `yaml:"prompt-cmd"`         // a script printing the instruction; runs after stage
	Stage      string            `yaml:"stage"`              // host script seeding the sandbox before it starts
	Sudo       *bool             `yaml:"sudo"`               // overrides the sandbox's sudo for this task's pairs
	Prepare    string            `yaml:"prepare"`            // script run inside the sandbox, as root, before the agent
	Collect    string            `yaml:"collect"`            // host script gathering outputs
	Timeout    string            `yaml:"timeout"`            // per-pair wall-clock limit (e.g. "15m"); "" -> 10m default; "0" -> none
	Env        map[string]string `yaml:"env"`                // exported to the task's host scripts
	Egress     TaskEgress        `yaml:"egress"`             // unioned over the sandbox's lists for this task's pairs
	// StepSandbox is the sandbox the host steps (stage, prompt-cmd, collect)
	// run in; "" runs them on the host. StepEnv names host variables passed
	// into it. see docs/spec/step-sandbox.md.
	StepSandbox string  `yaml:"step-sandbox"`
	StepEnv     StrList `yaml:"step-env"`
}

// TaskEgress is what a task adds to its sandbox's egress policy: entries for
// the hosts it needs and the urls that must stay out of reach. posture
// (none, default, deny-response, ...) is the sandbox's alone, so it has no
// field here and the unmarshal rejects it.
type TaskEgress struct {
	Set   bool     `yaml:"-"`
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

type taskEgressFields TaskEgress

// UnmarshalYAML checks the mapping's keys (the loader's unknown-field check
// stops at the item) and records that the task set one.
func (e *TaskEgress) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: egress: a task takes a mapping of allow and deny", node.Line)
	}
	known := yamlKeys(reflect.TypeOf(taskEgressFields{}))
	var errs errList
	for i := 0; i+1 < len(node.Content); i += 2 {
		if k := node.Content[i].Value; !known[k] {
			errs.add("line %d: egress: unknown field %q (a task sets only allow and deny; the rest is the sandbox's)", node.Content[i].Line, k)
		}
	}
	if err := errs.err(); err != nil {
		return err
	}
	var f taskEgressFields
	if err := node.Decode(&f); err != nil {
		return err
	}
	*e = TaskEgress(f)
	e.Set = true
	return nil
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
// scorer sets `score`, a script: exit 0 + first stdout line =
// float [0,1] or "na" (skip), exit != 0 = failure. an agent scorer sets
// `agent` plus one of the prompt keys instead: an agent judges the output.
type Scorer struct {
	Name       string `yaml:"-"`
	Vector     string `yaml:"-"`
	Dir        string `yaml:"-"`
	Score      string `yaml:"score"` // scoring script
	Agent      string `yaml:"agent"` // agent scorer: the judging agent
	Prompt     string `yaml:"prompt" text:"free"`
	PromptFile string `yaml:"prompt-file"`
	PromptCmd  string `yaml:"prompt-cmd"`
	// an agent scorer's steps and settings, each with a task's meaning: the
	// judge runs as an agent on a task synthesised from them.
	Stage   string            `yaml:"stage"`
	Prepare string            `yaml:"prepare"`
	Collect string            `yaml:"collect"`
	Sudo    *bool             `yaml:"sudo"`
	Timeout string            `yaml:"timeout"` // "" -> 5m; "0" -> none
	Env     map[string]string `yaml:"env"`
	Egress  TaskEgress        `yaml:"egress"`
	// StepSandbox and StepEnv place the scorer's host steps: score, or an
	// agent scorer's stage, prompt-cmd and collect (as a task's).
	StepSandbox string  `yaml:"step-sandbox"`
	StepEnv     StrList `yaml:"step-env"`
}

// IsAgent reports whether the scorer judges via an agent rather than a script.
func (s Scorer) IsAgent() bool { return s.Agent != "" }

// DefaultJudgeTimeout bounds a judge whose scorer sets no timeout.
const DefaultJudgeTimeout = 5 * time.Minute

// Task is the task an agent scorer's judge runs: the scorer's prompt, steps
// and settings, named and placed as the scorer is.
func (s Scorer) Task() Task {
	timeout := s.Timeout
	if timeout == "" {
		timeout = DefaultJudgeTimeout.String()
	}
	return Task{
		Name: s.Name, Vector: s.Vector, Dir: s.Dir,
		Prompt: s.Prompt, PromptFile: s.PromptFile, PromptCmd: s.PromptCmd,
		Stage: s.Stage, Sudo: s.Sudo, Prepare: s.Prepare, Collect: s.Collect,
		Timeout: timeout, Env: s.Env, Egress: s.Egress,
		StepSandbox: s.StepSandbox, StepEnv: s.StepEnv,
	}
}

// Suite is a named (agents, tasks, scorers) selection. both cross-products are
// implied within it: run = agents x tasks, score = tasks x scorers. each axis
// lists vector or item names of that kind; a vector name expands to its items.
// scorers may be empty (a run-only suite); agents and tasks may not.
type Suite struct {
	Name    string  `yaml:"-"`
	Vector  string  `yaml:"-"`
	Agents  StrList `yaml:"agents"`
	Tasks   StrList `yaml:"tasks"`
	Scorers StrList `yaml:"scorers"`
	// Manual leaves the suite out of a run that names no suite: it runs
	// only when -S names it, or a suite vector holding it.
	Manual bool `yaml:"manual"`
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
