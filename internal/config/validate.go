package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Validate checks a parsed config: ids, per-kind fields, cross-references,
// and that the suites expand cleanly. all problems are collected and returned
// together so one run surfaces every error.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.validateNames(add)
	c.validateSandboxes(add)
	c.validateAgents(add)
	c.validateTasks(add)
	c.validateScorers(add)
	c.validateSuites(add)

	// suite expansion doubles as referential validation (dangling refs,
	// duplicate ids after expansion).
	if _, err := c.ExpandTestPairs(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.ExpandScorePairs(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateNames enforces the namespace per kind: item ids unique, no id
// naming two different sets (a vector id equal to an item id is fine only
// when that item is the vector's sole member), no characters that would
// break a run dir, env var or template.
func (c *Config) validateNames(add func(string, ...any)) {
	for _, kind := range Kinds {
		items := map[string]bool{}
		vectors := map[string][]string{}
		for _, m := range c.members(kind) {
			if bad := badID(m.id); bad != "" {
				add("%s %q: id %s", kind, m.id, bad)
			}
			if items[m.id] {
				add("duplicate %s id: %s", kind, m.id)
			}
			items[m.id] = true
			vectors[m.vector] = append(vectors[m.vector], m.id)
		}
		for vec, ids := range vectors {
			if bad := badID(vec); bad != "" {
				add("vector %q: id %s", vec, bad)
			}
			if items[vec] && !(len(ids) == 1 && ids[0] == vec) {
				add("%s vector %q and %s %q share an id but name different things -- rename one", kind, vec, kind, vec)
			}
		}
	}
}

// badID says what is wrong with an id, or "".
func badID(id string) string {
	switch {
	case id == "":
		return "is empty"
	case strings.ContainsAny(id, "/$ \t\n"):
		return "may not contain /, $ or whitespace"
	}
	return ""
}

type member struct{ id, vector string }

// members lists (id, vector) for every item of the kind, in file order.
func (c *Config) members(kind string) []member {
	var out []member
	switch kind {
	case "sandbox":
		for _, x := range c.Sandboxes {
			out = append(out, member{x.ID, x.Vector})
		}
	case "agent":
		for _, x := range c.Agents {
			out = append(out, member{x.ID, x.Vector})
		}
	case "task":
		for _, x := range c.Tasks {
			out = append(out, member{x.ID, x.Vector})
		}
	case "scorer":
		for _, x := range c.Scorers {
			out = append(out, member{x.ID, x.Vector})
		}
	case "suite":
		for _, x := range c.Suites {
			out = append(out, member{x.ID, x.Vector})
		}
	}
	return out
}

func (c *Config) validateSandboxes(add func(string, ...any)) {
	for _, s := range c.Sandboxes {
		switch s.Driver {
		case "docker", "podman":
			if s.Image == "" && s.Build == "" {
				add("sandbox %q: needs an image or a build context", s.ID)
			}
			if s.Image != "" && s.Build != "" {
				add("sandbox %q: image and build are mutually exclusive", s.ID)
			}
		case "bwrap":
			add("sandbox %q: bwrap driver not implemented yet", s.ID)
		default:
			add("sandbox %q: unknown driver %q (docker|podman|bwrap)", s.ID, s.Driver)
		}
		validateEgress(add, s)
	}
}

// validateEgress checks a sandbox's egress policy so a bad mode fails at load,
// not mid-run.
func validateEgress(add func(string, ...any), s Sandbox) {
	switch s.Egress.Mode {
	case "", "open", "none", "proxy", "mitm-proxy":
	default:
		add("sandbox %q: unknown egress mode %q (open|none|proxy|mitm-proxy)", s.ID, s.Egress.Mode)
	}
	switch s.Egress.Default {
	case "", "deny", "allow":
	default:
		add("sandbox %q: unknown egress default %q (deny|allow)", s.ID, s.Egress.Default)
	}

	// plain allow/deny host lists only do anything once traffic is actually
	// filtered -- open/none/unset pass everything through regardless, so a
	// list here would silently never be consulted.
	switch s.Egress.Mode {
	case "", "open", "none":
		if len(s.Egress.Allow) > 0 || len(s.Egress.Deny) > 0 {
			add("sandbox %q: allow/deny needs egress mode proxy or mitm-proxy (got %q)", s.ID, s.Egress.Mode)
		}
	}

	// default picks which of allow/deny is live (deny -> allowlist, only
	// Allow consulted; allow -> denylist, only Deny consulted); a non-empty
	// entry on the other side would never be looked at.
	switch s.Egress.Default {
	case "allow":
		if len(s.Egress.Allow) > 0 {
			add("sandbox %q: allow is ignored under default: allow (a denylist) -- move these entries to deny, or set default: deny", s.ID)
		}
	case "", "deny":
		if len(s.Egress.Deny) > 0 {
			add("sandbox %q: deny is ignored under default: deny (an allowlist) -- move these entries to allow, or set default: allow", s.ID)
		}
	}

	// slice, not a map, so a joined error is deterministic (deny-urls before
	// allow-urls) rather than depending on go's random map iteration order.
	urlRules := []struct {
		field string
		pats  []string
	}{
		{"deny-urls", s.Egress.DenyURLs},
		{"allow-urls", s.Egress.AllowURLs},
	}
	for _, r := range urlRules {
		if len(r.pats) > 0 && s.Egress.Mode != "mitm-proxy" {
			add("sandbox %q: %s needs egress mode mitm-proxy (got %q)", s.ID, r.field, s.Egress.Mode)
		}
		for _, p := range r.pats {
			host, _, _ := strings.Cut(p, "/")
			// the host part picks which hosts get mitm'd -- a wildcard would
			// mitm everything (incl. the inference api), so it must be literal.
			if host == "" || strings.Contains(host, "*") {
				add("sandbox %q: %s pattern %q must start with a literal hostname", s.ID, r.field, p)
			}
		}
	}
}

func (c *Config) validateAgents(add func(string, ...any)) {
	for _, a := range c.Agents {
		switch {
		case a.Harness == "":
			add("agent %q: missing harness (%s)", a.ID, harnessList)
		case !Harnesses[a.Harness]:
			add("agent %q: unknown harness %q", a.ID, a.Harness)
		}
		if a.Model == "" {
			add("agent %q: model is required", a.ID)
		}
		if a.Effort != "" && !effortHarnesses[a.Harness] {
			add("agent %q: effort is not supported by harness %q", a.ID, a.Harness)
		}
		if _, err := c.ResolveSandbox(a); err != nil {
			add("%v", err)
		}
	}
}

// effortHarnesses is the set of harnesses whose cli takes a reasoning-effort
// flag (claude: --effort; codex: model_reasoning_effort; copilot: --effort;
// opencode: --variant).
var effortHarnesses = map[string]bool{
	"claude-cli-keyless":  true,
	"codex-cli-keyless":   true,
	"copilot-cli-keyless": true,
	"opencode-openrouter": true,
}

// ResolveSandbox returns the sandbox id an agent runs in: its `sandbox` (an
// item id, or a vector of one), or the only sandbox defined when omitted.
func (c *Config) ResolveSandbox(a Agent) (string, error) {
	if a.Sandbox != "" {
		id, err := c.One("sandbox", a.Sandbox)
		if err != nil {
			return "", fmt.Errorf("agent %q: %w", a.ID, err)
		}
		return id, nil
	}
	if len(c.Sandboxes) == 1 {
		return c.Sandboxes[0].ID, nil
	}
	return "", fmt.Errorf("agent %q: no sandbox set and %d defined -- name one with `sandbox:`", a.ID, len(c.Sandboxes))
}

func (c *Config) validateTasks(add func(string, ...any)) {
	for _, t := range c.Tasks {
		if t.Prompt == "" {
			add("task %q: prompt is required", t.ID)
		}
		if t.Timeout != "" {
			if _, err := time.ParseDuration(t.Timeout); err != nil {
				add("task %q: bad timeout %q (use e.g. 10m, 300s)", t.ID, t.Timeout)
			}
		}
	}
}

func (c *Config) validateScorers(add func(string, ...any)) {
	for _, s := range c.Scorers {
		switch {
		case s.Score != "" && s.AgentID != "":
			add("scorer %q: score and agent-id are mutually exclusive (a file scorer or an agent scorer, not both)", s.ID)
		case s.Score != "":
			if s.Prompt != "" {
				add("scorer %q: prompt is an agent-scorer field -- remove it, or replace score with agent-id", s.ID)
			}
		case s.AgentID != "":
			add("scorer %q: agent scorers are not implemented yet", s.ID)
			if _, err := c.One("agent", s.AgentID); err != nil {
				add("scorer %q: agent-id: %v", s.ID, err)
			}
			if s.Prompt == "" {
				add("scorer %q: an agent scorer needs a prompt", s.ID)
			}
		default:
			add("scorer %q: needs a score script or an agent-id", s.ID)
		}
	}
}

// validateSuites checks suite well-formedness (required axes) and that no
// item is orphaned -- a task/scorer/agent in no suite would silently never
// run, which is exactly the forgetting the explicit lists invite. exemption:
// an agent referenced by a scorer's agent-id legitimately lives outside every
// suite (it judges, it isn't judged).
func (c *Config) validateSuites(add func(string, ...any)) {
	agentSets, taskSets, scorerSets := c.sets("agent"), c.sets("task"), c.sets("scorer")
	inAgents, inTasks, inScorers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range c.Suites {
		if len(s.Agents) == 0 {
			add("suite %q: agents is required", s.ID)
		}
		if len(s.Tasks) == 0 {
			add("suite %q: tasks is required", s.ID)
		}
		// scorers may be empty: a run-only suite, scored elsewhere or not at all.
		mark(inAgents, s.Agents, agentSets)
		mark(inTasks, s.Tasks, taskSets)
		mark(inScorers, s.Scorers, scorerSets)
	}
	if len(c.Suites) == 0 {
		return // a config being assembled; nothing to orphan-check against
	}

	judges := map[string]bool{}
	for _, sc := range c.Scorers {
		for _, id := range agentSets[sc.AgentID] {
			judges[id] = true
		}
	}
	for _, a := range c.Agents {
		if !inAgents[a.ID] && !judges[a.ID] {
			add("agent %q is in no suite -- it would never run (add it to a suite, or reference it from a scorer's agent-id)", a.ID)
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

// mark records every item id an axis reaches through sets.
func mark(m map[string]bool, xs StrList, sets map[string][]string) {
	for _, x := range xs {
		for _, id := range sets[x] {
			m[id] = true
		}
	}
}
