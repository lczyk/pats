package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
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
	c.validateTaskEgress(add)

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
		// at most one way to name the image. none is fine: the run resolves
		// it to the published sandbox (docker) or provisions it (lxd).
		var ways []string
		if s.Image != "" {
			ways = append(ways, "image")
		}
		if s.Build != "" {
			ways = append(ways, "build")
		}
		if s.Base != "" || s.Provision != "" || s.ProvisionCmd != "" {
			ways = append(ways, "base / provision")
		}
		if len(ways) > 1 {
			add("sandbox %q: %s are mutually exclusive", s.ID, strings.Join(ways, " and "))
		}
		lxdOnly := map[string]bool{"remote": s.Remote != "", "vm": s.VM, "profiles": len(s.Profiles) > 0, "config": len(s.Config) > 0}
		switch s.Driver {
		case "docker", "podman":
			for _, k := range []string{"remote", "vm", "profiles", "config"} {
				if lxdOnly[k] {
					add("sandbox %q: %s is lxd-only (driver %q)", s.ID, k, s.Driver)
				}
			}
			// the docker agent runs as the host uid, with no passwd entry
			// for sudo to work from.
			if s.Sudo {
				add("sandbox %q: sudo is lxd-only for now (driver %q runs the agent as the host uid)", s.ID, s.Driver)
			}
			if s.Limits.Disk != "" {
				add("sandbox %q: limits.disk is lxd-only (driver %q)", s.ID, s.Driver)
			}
		case "lxd":
			if s.Build != "" {
				add("sandbox %q: build is a dockerfile, docker / podman only; lxd takes base and provision", s.ID)
			}
			for _, k := range sortedKeys(s.Config) {
				// these add devices the expanded config does not show, which
				// would void the egress topology.
				if strings.HasPrefix(k, "raw.") {
					add("sandbox %q: config %s is refused: raw config can add devices pats cannot check", s.ID, k)
				}
			}
		case "bwrap":
			add("sandbox %q: bwrap driver not implemented yet", s.ID)
		default:
			add("sandbox %q: unknown driver %q (docker|podman|lxd|bwrap)", s.ID, s.Driver)
		}
		if s.MaxParallel < 0 {
			add("sandbox %q: max-parallel must be positive", s.ID)
		}
		if s.Limits.CPU < 0 {
			add("sandbox %q: limits.cpu must be positive", s.ID)
		}
		for _, f := range [][2]string{{"memory", s.Limits.Memory}, {"disk", s.Limits.Disk}} {
			if f[1] != "" && !sizeRe.MatchString(f[1]) {
				add("sandbox %q: limits.%s %q is not a size (e.g. 8GiB)", s.ID, f[0], f[1])
			}
		}
		validateEgress(add, s)
		for _, k := range sortedKeys(s.Env) {
			switch {
			case !envName.MatchString(k):
				add("sandbox %q: env %q is not a valid variable name", s.ID, k)
			case strings.HasPrefix(k, "PATS_"):
				add("sandbox %q: env %q: PATS_* is reserved for pats", s.ID, k)
			case k == "PATH" || k == "HOME":
				add("sandbox %q: env %s is set by pats", s.ID, k)
			}
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateEgress checks a sandbox's egress policy so a bad entry fails at
// load, not mid-run. the entry grammar itself is the proxy's; this adds what
// the config layer knows: which driver, and which fields pats owns.
func validateEgress(add func(string, ...any), s Sandbox) {
	if !s.Egress.Set {
		return
	}
	if len(s.Egress.Harness) > 0 {
		add("sandbox %q: egress harness is set by pats, per agent", s.ID)
	}
	r, err := s.Egress.Policy.Rule()
	if err != nil {
		add("sandbox %q: egress: %v", s.ID, err)
		return
	}
	// under a denylist a bare allow entry can never matter: deny wins, and
	// everything else passes anyway. a url allow entry does (it restricts
	// its host to matching paths).
	if s.Egress.Default == "allow" {
		for _, e := range r.Allow {
			if !e.URL() {
				add("sandbox %q: egress allow %q is dead under default: allow -- a bare host passes anyway", s.ID, e.Raw)
			}
		}
	}
	if s.Egress.Image != "" && !s.IsContainer() {
		add("sandbox %q: egress image needs a container driver (driver %q runs the proxy in-process)", s.ID, s.Driver)
	}
}

// validateTaskEgress checks a task's egress entries, then every pair that
// runs the task: its rules only exist under a sandbox that runs the proxy,
// and a bare allow host is dead under a denylisting one.
func (c *Config) validateTaskEgress(add func(string, ...any)) {
	tasks := map[string]Task{}
	for _, t := range c.Tasks {
		tasks[t.ID] = t
		if !t.Egress.Set {
			continue
		}
		if _, err := (proxy.Policy{Allow: t.Egress.Allow, Deny: t.Egress.Deny}).Rule(); err != nil {
			add("task %q: egress: %v", t.ID, err)
		}
	}
	pairs, err := c.ExpandTestPairs()
	if err != nil {
		return // reported by Validate
	}
	sandboxes := map[string]Sandbox{}
	for _, s := range c.Sandboxes {
		sandboxes[s.ID] = s
	}
	agents := map[string]Agent{}
	for _, a := range c.Agents {
		agents[a.ID] = a
	}
	seen := map[[2]string]bool{}
	for _, p := range pairs {
		t := tasks[p.Task]
		if t.Sudo != nil && *t.Sudo {
			if sbID, err := c.ResolveSandbox(agents[p.Agent]); err == nil && sandboxes[sbID].Driver != "lxd" {
				add("task %q sets sudo, but agent %q runs it in sandbox %q (driver %q); sudo is lxd-only for now", p.Task, p.Agent, sbID, sandboxes[sbID].Driver)
			}
		}
		if !t.Egress.Set {
			continue
		}
		sbID, err := c.ResolveSandbox(agents[p.Agent])
		if err != nil || seen[[2]string{p.Task, sbID}] {
			continue
		}
		seen[[2]string{p.Task, sbID}] = true
		sb := sandboxes[sbID]
		if !sb.Egress.Set {
			add("task %q sets egress, but agent %q runs it in sandbox %q, whose egress is %s -- give the sandbox an egress mapping", p.Task, p.Agent, sbID, sb.Egress.Mode())
			continue
		}
		if sb.Egress.Default == "allow" {
			for _, line := range t.Egress.Allow {
				if e, err := proxy.ParseEntry(line, false); err == nil && !e.URL() {
					add("task %q: egress allow %q is dead under sandbox %q's default: allow", p.Task, line, sbID)
				}
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
		sbID, err := c.ResolveSandbox(a)
		if err != nil {
			add("%v", err)
			continue
		}
		// every harness reaches an inference api (agent.RequiredHosts), so a
		// sandbox with no network at all cannot run one.
		for _, s := range c.Sandboxes {
			if s.ID == sbID && s.Egress.None {
				add("agent %q: harness %q needs the network, but sandbox %q has egress: none", a.ID, a.Harness, s.ID)
			}
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
		if t.Prompt == "" && t.PromptFile == "" && t.PromptCmd == "" {
			add("task %q: needs one of prompt, prompt-file or prompt-cmd", t.ID)
		}
		if t.Timeout != "" {
			if _, err := time.ParseDuration(t.Timeout); err != nil {
				add("task %q: bad timeout %q (use e.g. 10m, 300s)", t.ID, t.Timeout)
			}
		}
		names := make([]string, 0, len(t.Env))
		for k := range t.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			switch {
			case !envName.MatchString(k):
				add("task %q: env %q is not a valid variable name", t.ID, k)
			case strings.HasPrefix(k, "PATS_"):
				add("task %q: env %q: PATS_* is reserved for pats", t.ID, k)
			case k == "PATH":
				add("task %q: env PATH is set by pats (bin/ is prepended to it)", t.ID)
			}
		}
	}
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sizeRe is a size as lxd and docker both read it: a number and a binary or
// decimal unit.
var sizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)

func (c *Config) validateScorers(add func(string, ...any)) {
	for _, s := range c.Scorers {
		script := s.Score != "" || s.ScoreCmd != ""
		prompt := s.Prompt != "" || s.PromptFile != "" || s.PromptCmd != ""
		switch {
		case script && s.IsAgent():
			add("scorer %q: score / score-cmd and agent are mutually exclusive (a script scorer or an agent scorer, not both)", s.ID)
		case script:
			if prompt {
				add("scorer %q: a prompt is an agent-scorer field -- remove it, or replace the score with agent", s.ID)
			}
		case s.IsAgent():
			add("scorer %q: agent scorers are not implemented yet", s.ID)
			if _, err := c.One("agent", s.Agent); err != nil {
				add("scorer %q: agent: %v", s.ID, err)
			}
			if !prompt {
				add("scorer %q: an agent scorer needs one of prompt, prompt-file or prompt-cmd", s.ID)
			}
		default:
			add("scorer %q: needs score, score-cmd or agent", s.ID)
		}
	}
}

// validateSuites checks suite well-formedness (required axes) and that no
// item is orphaned -- a task/scorer/agent in no suite would silently never
// run, which is exactly the forgetting the explicit lists invite. exemption:
// an agent referenced by a scorer's agent legitimately lives outside every
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
		for _, id := range agentSets[sc.Agent] {
			judges[id] = true
		}
	}
	for _, a := range c.Agents {
		if !inAgents[a.ID] && !judges[a.ID] {
			add("agent %q is in no suite -- it would never run (add it to a suite, or reference it from a scorer's agent)", a.ID)
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
