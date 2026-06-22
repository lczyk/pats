// Package config loads, validates, and expands a pats.yaml.
//
// the file declares four "vectors" -- sandboxes, agents, tasks, scorers --
// plus two matrices (test, scorer) that cross-product agents/tasks/scorers
// into the pairs the run + score phases drive.
package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is a parsed pats.yaml. it is not normalised on load -- defaults
// (driver, single-sandbox) resolve via the helper methods, and Validate
// checks the whole thing.
type Config struct {
	Sandboxes    []Sandbox `yaml:"sandboxes"`
	Agents       []Agent   `yaml:"agents"`
	Tasks        []Task    `yaml:"tasks"`
	Scorers      []Scorer  `yaml:"scorers"`
	TestMatrix   []Row     `yaml:"test-matrix"`
	ScorerMatrix []Row     `yaml:"scorer-matrix"`
}

// Sandbox is an isolation environment a task-running agent executes in.
type Sandbox struct {
	ID     string `yaml:"id"`
	Kind   string `yaml:"kind"`   // container | bwrap
	Driver string `yaml:"driver"` // container: docker|podman (defaults docker); bwrap: bwrap
	Image  string `yaml:"image"`  // container only
}

// ResolvedDriver fills the per-kind default when driver is omitted.
func (s Sandbox) ResolvedDriver() string {
	if s.Driver != "" {
		return s.Driver
	}
	switch s.Kind {
	case "container":
		return "docker"
	case "bwrap":
		return "bwrap"
	}
	return ""
}

// Agent is a harness (an agent cli) under test, run in a sandbox. two kinds:
//
//	opencode-openrouter -- opencode cli, models via openrouter (OPENROUTER_API_KEY)
//	claude-cli-keyless  -- claude cli, keyless oauth creds (~/.claude/.credentials.json)
type Agent struct {
	ID      string `yaml:"id"`
	Kind    string `yaml:"kind"`
	Model   string `yaml:"model"`
	Sandbox string `yaml:"sandbox"`
}

// Task is one scenario handed to a task-running agent.
type Task struct {
	ID         string `yaml:"id"`
	PromptFile string `yaml:"prompt-file"` // the instruction
	Prepare    string `yaml:"prepare"`     // seed the sandbox (optional)
	Collect    string `yaml:"collect"`     // gather outputs (optional)
}

// Scorer scores one aspect of a task's collected output, 0.0 - 1.0.
//
//	bash  -- run a script
//	agent -- ask an agent to judge
type Scorer struct {
	ID         string `yaml:"id"`
	Kind       string `yaml:"kind"`        // bash | agent
	File       string `yaml:"file"`        // bash
	AgentID    string `yaml:"agent-id"`    // agent
	PromptFile string `yaml:"prompt-file"` // agent
}

// Row is one cross-product row of a matrix. Agent/Task/Scorer each take a
// scalar, a list, or "*" (all); Weight defaults to 1.0 when omitted.
type Row struct {
	Agent  StrList  `yaml:"agent"`
	Task   StrList  `yaml:"task"`
	Scorer StrList  `yaml:"scorer"`
	Weight *float64 `yaml:"weight"`
}

// WeightOr returns the row weight, or def when omitted.
func (r Row) WeightOr(def float64) float64 {
	if r.Weight == nil {
		return def
	}
	return *r.Weight
}

// StrList accepts a yaml scalar or sequence and stores it as []string. the
// sentinel "*" (a single-element list) means "all", resolved at expansion.
type StrList []string

func (s *StrList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var single string
		if err := node.Decode(&single); err != nil {
			return err
		}
		*s = StrList{single}
	case yaml.SequenceNode:
		var many []string
		if err := node.Decode(&many); err != nil {
			return err
		}
		*s = StrList(many)
	default:
		return fmt.Errorf("expected a scalar or list, got yaml kind %d", node.Kind)
	}
	return nil
}

// Load reads + parses a pats.yaml. unknown fields are rejected so typos fail
// loudly rather than being silently ignored. it does not Validate.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse pats.yaml: %w", err)
	}
	return &c, nil
}
