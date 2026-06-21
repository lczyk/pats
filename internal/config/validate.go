package config

import (
	"errors"
	"fmt"
)

// Validate checks the whole config: vector well-formedness, cross-references,
// kind/field consistency, and that both matrices expand cleanly. all problems
// are collected and returned together so one run surfaces every error.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	sandboxes := c.validateSandboxes(add)
	agents := c.validateAgents(add, sandboxes)
	tasks := c.validateTasks(add)
	c.validateScorers(add, agents)

	// matrix expansion doubles as referential validation (dangling refs,
	// api-in-test-matrix, dup pairs, "*" against empty vectors).
	if _, err := c.ExpandTestMatrix(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.ExpandScorerMatrix(); err != nil {
		errs = append(errs, err)
	}
	_ = tasks

	return errors.Join(errs...)
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
			if s.Image == "" {
				add("sandbox %q: container kind needs an image", s.ID)
			}
		case "bwrap":
			if s.Image != "" {
				add("sandbox %q: bwrap kind takes no image", s.ID)
			}
		case "":
			add("sandbox %q: missing kind (container|bwrap)", s.ID)
		default:
			add("sandbox %q: unknown kind %q", s.ID, s.Kind)
		}
	}
	return out
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

		switch a.Kind {
		case "harness":
			if a.Provider == "" {
				add("agent %q: harness needs a provider", a.ID)
			}
			if a.Command != "" {
				add("agent %q: harness must not set command (that's adhoc)", a.ID)
			}
			if a.Key != "" || a.BaseURL != "" {
				add("agent %q: harness must not set key/base-url (that's api)", a.ID)
			}
			c.checkSandboxRef(add, a, sandboxes)
		case "adhoc":
			if a.Command == "" {
				add("agent %q: adhoc needs a command", a.ID)
			}
			if a.Provider != "" {
				add("agent %q: adhoc must not set provider (it has no preset)", a.ID)
			}
			if a.Key != "" || a.BaseURL != "" {
				add("agent %q: adhoc must not set key/base-url (that's api)", a.ID)
			}
			c.checkSandboxRef(add, a, sandboxes)
		case "api":
			if a.Provider == "" {
				add("agent %q: api needs a provider", a.ID)
			}
			if a.Sandbox != "" {
				add("agent %q: api agent is scorer-only and must not set a sandbox", a.ID)
			}
			if a.Command != "" {
				add("agent %q: api must not set command", a.ID)
			}
		case "":
			add("agent %q: missing kind (harness|adhoc|api)", a.ID)
		default:
			add("agent %q: unknown kind %q", a.ID, a.Kind)
		}
	}
	return out
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
		if t.PromptFile == "" {
			add("task %q: prompt-file is required", t.ID)
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
		case "bash":
			if s.File == "" {
				add("scorer %q: bash kind needs a file", s.ID)
			}
		case "agent":
			if s.AgentID == "" {
				add("scorer %q: agent kind needs an agent-id", s.ID)
			} else if _, ok := agents[s.AgentID]; !ok {
				add("scorer %q: unknown agent-id %q", s.ID, s.AgentID)
			}
			if s.PromptFile == "" {
				add("scorer %q: agent kind needs a prompt-file", s.ID)
			}
		case "":
			add("scorer %q: missing kind (bash|agent)", s.ID)
		default:
			add("scorer %q: unknown kind %q", s.ID, s.Kind)
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
