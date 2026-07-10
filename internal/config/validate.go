package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Validate checks the whole config: vector well-formedness, cross-references,
// kind/field consistency, and that the suites expand cleanly. all problems
// are collected and returned together so one run surfaces every error.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	sandboxes := c.validateSandboxes(add)
	agents := c.validateAgents(add, sandboxes)
	c.validateTasks(add)
	scorers := c.validateScorers(add, agents)
	c.validateSuites(add, scorers)

	// suite expansion doubles as referential validation (dangling refs,
	// in-suite duplicate ids).
	if _, err := c.ExpandTestPairs(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.ExpandScorePairs(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateSuites checks suite well-formedness (ids, required axes) and that no
// vector entry is orphaned -- a task/scorer/agent in no suite would silently
// never run, which is exactly the forgetting the explicit lists invite.
// exemption: an agent referenced by a `kind: agent` scorer legitimately lives
// outside every suite (it judges, it isn't judged).
func (c *Config) validateSuites(add func(string, ...any), scorers map[string]Scorer) {
	seen := map[string]bool{}
	inAgents, inTasks, inScorers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range c.Suites {
		if s.ID == "" {
			add("suite with empty id")
			continue
		}
		if seen[s.ID] {
			add("duplicate suite id: %s", s.ID)
		}
		seen[s.ID] = true
		if len(s.Agents) == 0 {
			add("suite %q: agents is required", s.ID)
		}
		if len(s.Tasks) == 0 {
			add("suite %q: tasks is required", s.ID)
		}
		// scorers may be empty: a run-only suite, scored elsewhere or not at all.
		mark(inAgents, s.Agents)
		mark(inTasks, s.Tasks)
		mark(inScorers, s.Scorers)
	}
	if len(c.Suites) == 0 {
		return // vectors-only config (e.g. being assembled); nothing to orphan-check against
	}

	judges := map[string]bool{}
	for _, sc := range scorers {
		if sc.Kind == "agent" && sc.AgentID != "" {
			judges[sc.AgentID] = true
		}
	}
	for _, a := range c.Agents {
		if !inAgents[a.ID] && !judges[a.ID] {
			add("agent %q is in no suite -- it would never run (add it to a suite, or reference it from a kind: agent scorer)", a.ID)
		}
	}
	for _, t := range c.Tasks {
		if !inTasks[t.ID] {
			add("task %q is in no suite -- it would never run", t.ID)
		}
	}
	for _, s := range c.Scorers {
		if !inScorers[s.ID] {
			add("scorer %q is in no suite -- it would never run", s.ID)
		}
	}
}

func mark(m map[string]bool, xs StrList) {
	for _, x := range xs {
		m[x] = true
	}
}

func (c *Config) validateSandboxes(add func(string, ...any)) map[string]Sandbox {
	out := map[string]Sandbox{}
	for _, s := range c.Sandboxes {
		if s.ID == "" {
			add("sandbox with empty id")
			continue
		}
		if _, dup := out[s.ID]; dup {
			add("duplicate sandbox id: %s", s.ID)
		}
		out[s.ID] = s
		switch s.Kind {
		case "container":
			if s.Image == "" && s.Build == "" {
				add("sandbox %q: container kind needs an image or a build context", s.ID)
			}
			if s.Image != "" && s.Build != "" {
				add("sandbox %q: image and build are mutually exclusive", s.ID)
			}
		case "bwrap":
			add("sandbox %q: bwrap kind not implemented yet", s.ID)
		case "":
			add("sandbox %q: missing kind (container|bwrap)", s.ID)
		default:
			add("sandbox %q: unknown kind %q", s.ID, s.Kind)
		}
		validateEgress(add, s)
	}
	return out
}

// validateEgress checks a sandbox's egress policy so a bad mode fails at load,
// not mid-run.
func validateEgress(add func(string, ...any), s Sandbox) {
	switch s.Egress.Mode {
	case "", "open", "none", "proxy", "mitm-proxy":
	case "off":
		add("sandbox %q: egress mode %q was renamed -- use `open`", s.ID, s.Egress.Mode)
	default:
		add("sandbox %q: unknown egress mode %q (open|none|proxy|mitm-proxy)", s.ID, s.Egress.Mode)
	}
	switch s.Egress.Default {
	case "", "deny", "allow":
	default:
		add("sandbox %q: unknown egress default %q (deny|allow)", s.ID, s.Egress.Default)
	}
	urlRules := map[string][]string{"deny-urls": s.Egress.DenyURLs, "allow-urls": s.Egress.AllowURLs}
	for field, pats := range urlRules {
		if len(pats) > 0 && s.Egress.Mode != "mitm-proxy" {
			add("sandbox %q: %s needs egress mode mitm-proxy (got %q)", s.ID, field, s.Egress.Mode)
		}
		for _, p := range pats {
			host, _, _ := strings.Cut(p, "/")
			// the host part picks which hosts get mitm'd -- a wildcard would
			// mitm everything (incl. the inference api), so it must be literal.
			if host == "" || strings.Contains(host, "*") {
				add("sandbox %q: %s pattern %q must start with a literal hostname", s.ID, field, p)
			}
		}
	}
}

func (c *Config) validateAgents(add func(string, ...any), sandboxes map[string]Sandbox) map[string]Agent {
	out := map[string]Agent{}
	for _, a := range c.Agents {
		if a.ID == "" {
			add("agent with empty id")
			continue
		}
		if _, dup := out[a.ID]; dup {
			add("duplicate agent id: %s", a.ID)
		}
		out[a.ID] = a

		switch {
		case a.Kind == "":
			add("agent %q: missing kind (opencode-openrouter|claude-cli-keyless|codex-cli-keyless)", a.ID)
		case !AgentKinds[a.Kind]:
			add("agent %q: unknown kind %q", a.ID, a.Kind)
		}
		if a.Model == "" {
			add("agent %q: model is required", a.ID)
		}
		if a.Effort != "" && !effortKinds[a.Kind] {
			add("agent %q: effort is not supported by kind %q", a.ID, a.Kind)
		}
		c.checkSandboxRef(add, a, sandboxes)
	}
	return out
}

// AgentKinds is the set of supported agent kinds. the kind->command mapping
// lives in agent.HarnessCmds; config can't import it (would cycle), so the
// names are repeated here -- a test in the agent package keeps the two in sync.
var AgentKinds = map[string]bool{
	"opencode-openrouter": true,
	"claude-cli-keyless":  true,
	"codex-cli-keyless":   true,
}

// effortKinds is the set of agent kinds whose cli takes a reasoning-effort
// flag (claude: --effort; codex: model_reasoning_effort; opencode: --variant).
var effortKinds = map[string]bool{
	"claude-cli-keyless":  true,
	"codex-cli-keyless":   true,
	"opencode-openrouter": true,
}

// checkSandboxRef validates the agent's sandbox selection (explicit id must
// exist; omitted is fine only when exactly one sandbox is defined).
func (c *Config) checkSandboxRef(add func(string, ...any), a Agent, sandboxes map[string]Sandbox) {
	if a.Sandbox != "" {
		if _, ok := sandboxes[a.Sandbox]; !ok {
			add("agent %q: unknown sandbox %q", a.ID, a.Sandbox)
		}
		return
	}
	if len(c.Sandboxes) != 1 {
		add("agent %q: no sandbox set and %d defined -- name one with `sandbox:`", a.ID, len(c.Sandboxes))
	}
}

func (c *Config) validateTasks(add func(string, ...any)) map[string]Task {
	out := map[string]Task{}
	for _, t := range c.Tasks {
		if t.ID == "" {
			add("task with empty id")
			continue
		}
		if _, dup := out[t.ID]; dup {
			add("duplicate task id: %s", t.ID)
		}
		out[t.ID] = t
		if t.Prompt == "" {
			add("task %q: prompt is required", t.ID)
		}
		if t.Timeout != "" {
			if _, err := time.ParseDuration(t.Timeout); err != nil {
				add("task %q: bad timeout %q (use e.g. 10m, 300s)", t.ID, t.Timeout)
			}
		}
	}
	return out
}

func (c *Config) validateScorers(add func(string, ...any), agents map[string]Agent) map[string]Scorer {
	out := map[string]Scorer{}
	for _, s := range c.Scorers {
		if s.ID == "" {
			add("scorer with empty id")
			continue
		}
		if _, dup := out[s.ID]; dup {
			add("duplicate scorer id: %s", s.ID)
		}
		out[s.ID] = s
		switch s.Kind {
		case "":
			if s.Score == "" {
				add("scorer %q: needs a score script", s.ID)
			}
		case "agent":
			add("scorer %q: agent kind not implemented yet", s.ID)
			if s.AgentID == "" {
				add("scorer %q: agent kind needs an agent-id", s.ID)
			} else if _, ok := agents[s.AgentID]; !ok {
				add("scorer %q: unknown agent-id %q", s.ID, s.AgentID)
			}
			if s.Prompt == "" {
				add("scorer %q: agent kind needs a prompt", s.ID)
			}
		default:
			add("scorer %q: unknown kind %q (omit kind for an exec scorer, or use agent)", s.ID, s.Kind)
		}
	}
	return out
}

// ResolveSandbox returns the sandbox id an agent runs in, applying the
// single-sandbox default. errors mirror checkSandboxRef.
func (c *Config) ResolveSandbox(a Agent) (string, error) {
	if a.Sandbox != "" {
		return a.Sandbox, nil
	}
	if len(c.Sandboxes) == 1 {
		return c.Sandboxes[0].ID, nil
	}
	return "", fmt.Errorf("agent %q: no sandbox set and %d defined", a.ID, len(c.Sandboxes))
}
