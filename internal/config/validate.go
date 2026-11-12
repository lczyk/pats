package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// Validate checks a parsed config: names, per-kind fields, cross-references,
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
	c.validateSteps(add)

	// suite expansion doubles as referential validation (dangling refs,
	// duplicate names after expansion).
	if _, err := c.ExpandTestPairs(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.ExpandScorePairs(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateNames enforces the namespace per kind: item names unique, no name
// naming two different sets (a vector named as an item is fine only when
// that item is the vector's sole member), and every name of nameRe's shape.
func (c *Config) validateNames(add func(string, ...any)) {
	for _, kind := range Kinds {
		items := map[string]bool{}
		vectors := map[string][]string{}
		for _, m := range c.members(kind) {
			if bad := badName(m.name); bad != "" {
				add("%s %q %s", kind, m.name, bad)
			}
			if kind == "agent" && runDirFiles[m.name] {
				add("agent %q: the name is taken by a run's own %s, beside its agents' dirs", m.name, m.name)
			}
			if items[m.name] {
				add("duplicate %s name: %s", kind, m.name)
			}
			items[m.name] = true
			vectors[m.vector] = append(vectors[m.vector], m.name)
		}
		for vec, names := range vectors {
			if bad := badName(vec); bad != "" {
				add("vector %q %s", vec, bad)
			}
			if items[vec] && !(len(names) == 1 && names[0] == vec) {
				add("%s vector %q and %s %q share a name but are different things -- rename one", kind, vec, kind, vec)
			}
		}
	}
}

// nameRe is a name's shape, chisel's for a package: lowercase letters,
// digits, dots, pluses and single hyphens, a letter or digit first, no hyphen
// last, at least 2 long. no name starts with a dot, so none meets .judge or
// a judgement's staging dir.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:-?[.a-z0-9+]){1,}$`)

// runDirFiles are what pats writes at the top of a run dir, beside one dir
// per agent: no agent may be named as one.
var runDirFiles = map[string]bool{"scores.json": true}

// badName says what is wrong with a name, or "".
func badName(name string) string {
	switch {
	case name == "":
		return "is empty"
	case !nameRe.MatchString(name):
		return "is not a name: lowercase letters, digits, dots, pluses and single hyphens, a letter or digit first, no hyphen last, at least 2 long"
	}
	return ""
}

type member struct{ name, vector string }

// members lists (name, vector) for every item of the kind, in file order.
func (c *Config) members(kind string) []member {
	var out []member
	switch kind {
	case "sandbox":
		for _, x := range c.Sandboxes {
			out = append(out, member{x.Name, x.Vector})
		}
	case "agent":
		for _, x := range c.Agents {
			out = append(out, member{x.Name, x.Vector})
		}
	case "task":
		for _, x := range c.Tasks {
			out = append(out, member{x.Name, x.Vector})
		}
	case "scorer":
		for _, x := range c.Scorers {
			out = append(out, member{x.Name, x.Vector})
		}
	case "suite":
		for _, x := range c.Suites {
			out = append(out, member{x.Name, x.Vector})
		}
	}
	return out
}

// knownDrivers are every driver pats has, whether or not this build does:
// one a build leaves out is named as such, not as unknown.
var knownDrivers = map[string]bool{"docker": true, "podman": true, "lxd": true, "bwrap": true, "host": true}

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
		if s.Base != "" || s.Provision != "" {
			ways = append(ways, "base / provision")
		}
		if len(ways) > 1 {
			add("sandbox %q: %s are mutually exclusive", s.Name, strings.Join(ways, " and "))
		}
		lxdOnly := map[string]bool{"remote": s.Remote != "", "vm": s.VM, "profiles": len(s.Profiles) > 0, "config": len(s.Config) > 0}
		built := slices.Contains(sandbox.Drivers(), s.Driver)
		if !built && knownDrivers[s.Driver] {
			add("sandbox %q: driver %q is not in this build", s.Name, s.Driver)
		}
		switch s.Driver {
		case "docker", "podman":
			for _, k := range []string{"remote", "vm", "profiles", "config"} {
				if lxdOnly[k] {
					add("sandbox %q: %s is lxd-only (driver %q)", s.Name, k, s.Driver)
				}
			}
			// the docker agent runs as the host uid, with no passwd entry
			// for sudo to work from.
			if s.Sudo {
				add("sandbox %q: sudo is lxd-only for now (driver %q runs the agent as the host uid)", s.Name, s.Driver)
			}
			if s.Limits.Disk != "" {
				add("sandbox %q: limits.disk is lxd-only (driver %q)", s.Name, s.Driver)
			}
		case "lxd":
			if s.Build != "" {
				add("sandbox %q: build is a dockerfile, docker / podman only; lxd takes base and provision", s.Name)
			}
			for _, k := range sortedKeys(s.Config) {
				// these add devices the expanded config does not show, which
				// would void the egress topology.
				if strings.HasPrefix(k, "raw.") {
					add("sandbox %q: config %s is refused: raw config can add devices pats cannot check", s.Name, k)
				}
			}
		case "bwrap":
			validateBwrap(add, s)
		case "host":
			validateHost(add, s)
		default:
			add("sandbox %q: unknown driver %q (docker|podman|lxd|bwrap|host)", s.Name, s.Driver)
		}
		if s.SandNotBoxed && built && !sandbox.TraitsOf(s.Driver).Unboxed {
			add("sandbox %q: sand-not-boxed is the host driver's (driver %q)", s.Name, s.Driver)
		}
		if s.MaxParallel < 0 {
			add("sandbox %q: max-parallel must be positive", s.Name)
		}
		if s.Limits.CPU < 0 {
			add("sandbox %q: limits.cpu must be positive", s.Name)
		}
		for _, f := range [][2]string{{"memory", s.Limits.Memory}, {"disk", s.Limits.Disk}} {
			if f[1] != "" && !sizeRe.MatchString(f[1]) {
				add("sandbox %q: limits.%s %q is not a size (e.g. 8GiB)", s.Name, f[0], f[1])
			}
		}
		validateEgress(add, s)
		for _, k := range sortedKeys(s.Env) {
			switch {
			case !envName.MatchString(k):
				add("sandbox %q: env %q is not a valid variable name", s.Name, k)
			case strings.HasPrefix(k, "PATS_"):
				add("sandbox %q: env %q: PATS_* is reserved for pats", s.Name, k)
			case k == "PATH" || k == "HOME":
				add("sandbox %q: env %s is set by pats", s.Name, k)
			case shellReadonly[k]:
				add("sandbox %q: env %s is read-only in a shell, which hands the env to the command", s.Name, k)
			}
		}
	}
}

// validateBwrap refuses what a bwrap sandbox cannot honour, each key by
// name: it runs a registry image as it is, with one uid and no limits.
func validateBwrap(add func(string, ...any), s Sandbox) {
	refused := []struct {
		key string
		set bool
	}{
		{"build", s.Build != ""}, {"base", s.Base != ""}, {"provision", s.Provision != ""},
		{"limits", s.Limits != (Limits{})}, {"sudo", s.Sudo}, {"vm", s.VM}, {"remote", s.Remote != ""},
		{"profiles", len(s.Profiles) > 0}, {"config", len(s.Config) > 0},
	}
	for _, r := range refused {
		if r.set {
			add("sandbox %q: %s is refused under the bwrap driver: it runs a registry image as it is, as one uid, with no limits", s.Name, r.key)
		}
	}
}

// validateHost refuses what a host cannot honour, each key by name, and asks
// for the acknowledgement that the agent runs unboxed.
func validateHost(add func(string, ...any), s Sandbox) {
	if !s.SandNotBoxed {
		add("sandbox %q: the host driver runs the agent on this machine, as you, with no isolation; set sand-not-boxed: true to accept that", s.Name)
	}
	refused := []struct {
		key string
		set bool
	}{
		{"image", s.Image != ""}, {"build", s.Build != ""}, {"base", s.Base != ""}, {"provision", s.Provision != ""},
		{"limits", s.Limits != (Limits{})}, {"sudo", s.Sudo}, {"vm", s.VM}, {"remote", s.Remote != ""},
		{"profiles", len(s.Profiles) > 0}, {"config", len(s.Config) > 0}, {"egress: none", s.Egress.None},
	}
	for _, r := range refused {
		if r.set {
			add("sandbox %q: %s is refused under the host driver: the agent runs on this machine as it is", s.Name, r.key)
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
		add("sandbox %q: egress harness is set by pats, per agent", s.Name)
	}
	r, err := s.Egress.Policy.Rule()
	if err != nil {
		add("sandbox %q: egress: %v", s.Name, err)
		return
	}
	// under a denylist a bare allow entry can never matter: deny wins, and
	// everything else passes anyway. a url allow entry does (it restricts
	// its host to matching paths).
	if s.Egress.Default == "allow" {
		for _, e := range r.Allow {
			if !e.URL() {
				add("sandbox %q: egress allow %q is dead under default: allow -- a bare host passes anyway", s.Name, e.Raw)
			}
		}
	}
	if s.Egress.Image != "" && !s.IsContainer() {
		add("sandbox %q: egress image needs a container driver (driver %q runs the proxy in-process)", s.Name, s.Driver)
	}
}

// validateTaskEgress checks a task's egress entries, then every pair that
// runs the task: its rules only exist under a sandbox that runs the proxy,
// and a bare allow host is dead under a denylisting one.
func (c *Config) validateTaskEgress(add func(string, ...any)) {
	tasks := map[string]Task{}
	for _, t := range c.Tasks {
		tasks[t.Name] = t
		if !t.Egress.Set {
			continue
		}
		if _, err := (proxy.Policy{Allow: t.Egress.Allow, Deny: t.Egress.Deny}).Rule(); err != nil {
			add("task %q: egress: %v", t.Name, err)
		}
	}
	pairs, err := c.ExpandTestPairs()
	if err != nil {
		return // reported by Validate
	}
	sandboxes := map[string]Sandbox{}
	for _, s := range c.Sandboxes {
		sandboxes[s.Name] = s
	}
	agents := map[string]Agent{}
	for _, a := range c.Agents {
		agents[a.Name] = a
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
		h := c.HarnessOf(a)
		switch {
		case a.Harness == "":
			add("agent %q: missing harness (%s)", a.Name, strings.Join(c.harnessNames(), "|"))
		case h == nil:
			add("agent %q: unknown harness %q", a.Name, a.Harness)
		}
		if a.Model == "" {
			add("agent %q: model is required", a.Name)
		}
		if a.MaxCost != nil {
			if err := CheckCost(*a.MaxCost); err != nil {
				add("agent %q: max-cost: %v", a.Name, err)
			}
		}
		sbID, err := c.ResolveSandbox(a)
		if err != nil {
			add("%v", err)
			continue
		}
		// a harness that allows itself a host (its inference api) cannot run
		// in a sandbox with no network at all.
		for _, s := range c.Sandboxes {
			if s.Name == sbID && s.Egress.None && h != nil && len(h.Egress.Allow) > 0 {
				add("agent %q: harness %q needs the network, but sandbox %q has egress: none", a.Name, a.Harness, s.Name)
			}
		}
	}
}

// harnessNames lists the harnesses an agent may name, sorted.
func (c *Config) harnessNames() []string {
	if c.Harnesses == nil {
		return builtins().Names()
	}
	return c.Harnesses.Names()
}

// ResolveSandbox returns the sandbox name an agent runs in: its `sandbox` (an
// item name, or a vector of one), or the only sandbox defined when omitted.
func (c *Config) ResolveSandbox(a Agent) (string, error) {
	if a.Sandbox != "" {
		name, err := c.One("sandbox", a.Sandbox)
		if err != nil {
			return "", fmt.Errorf("agent %q: %w", a.Name, err)
		}
		return name, nil
	}
	if len(c.Sandboxes) == 1 {
		return c.Sandboxes[0].Name, nil
	}
	return "", fmt.Errorf("agent %q: no sandbox set and %d defined -- name one with `sandbox:`", a.Name, len(c.Sandboxes))
}

func (c *Config) validateTasks(add func(string, ...any)) {
	for _, t := range c.Tasks {
		if t.Prompt == "" && t.PromptFile == "" && t.PromptCmd == "" {
			add("task %q: needs one of prompt, prompt-file or prompt-cmd", t.Name)
		}
		if t.Timeout != "" {
			if d, err := time.ParseDuration(t.Timeout); err != nil || d < 0 {
				add("task %q: bad timeout %q (use e.g. 10m, 300s)", t.Name, t.Timeout)
			}
		}
		checkEnv(add, fmt.Sprintf("task %q", t.Name), t.Env)
	}
}

// checkEnv checks a task's or an agent scorer's env: names a shell can set,
// clear of what pats sets.
func checkEnv(add func(string, ...any), what string, env map[string]string) {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch {
		case !envName.MatchString(k):
			add("%s: env %q is not a valid variable name", what, k)
		case strings.HasPrefix(k, "PATS_"):
			add("%s: env %q: PATS_* is reserved for pats", what, k)
		case k == "PATH":
			add("%s: env PATH is set by pats (bin/ is prepended to it)", what)
		case shellReadonly[k]:
			add("%s: env %s is read-only in a shell, which hands the env to prepare", what, k)
		}
	}
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellReadonly are names a shell (bash as sh) will not let a script set:
// the env reaches a sandboxed command through one, so they cannot be passed.
var shellReadonly = map[string]bool{"UID": true, "EUID": true, "PPID": true, "SHELLOPTS": true, "BASHOPTS": true, "BASH_VERSINFO": true}

// sizeRe is a size as lxd and docker both read it: a number and a binary or
// decimal unit.
var sizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)

func (c *Config) validateScorers(add func(string, ...any)) {
	for _, s := range c.Scorers {
		script := s.Score != ""
		prompt := s.Prompt != "" || s.PromptFile != "" || s.PromptCmd != ""
		switch {
		case script && s.IsAgent():
			add("scorer %q: score and agent are mutually exclusive (a script scorer or an agent scorer, not both)", s.Name)
		case script:
			if prompt {
				add("scorer %q: a prompt is an agent-scorer field -- remove it, or replace the score with agent", s.Name)
			}
			for _, f := range judgeFields(s) {
				add("scorer %q: %s is an agent-scorer field -- a script scorer runs score alone", s.Name, f)
			}
		case s.IsAgent():
			c.validateJudge(add, s)
			if !prompt {
				add("scorer %q: an agent scorer needs one of prompt, prompt-file or prompt-cmd", s.Name)
			}
		default:
			add("scorer %q: needs score or agent", s.Name)
		}
	}
}

// judgeFields names the agent-scorer fields s sets besides its agent and prompt.
func judgeFields(s Scorer) []string {
	var set []string
	for _, f := range []struct {
		name string
		on   bool
	}{
		{"stage", s.Stage != ""}, {"prepare", s.Prepare != ""}, {"collect", s.Collect != ""},
		{"env", len(s.Env) > 0}, {"sudo", s.Sudo != nil}, {"egress", s.Egress.Set}, {"timeout", s.Timeout != ""},
	} {
		if f.on {
			set = append(set, f.name)
		}
	}
	return set
}

// validateJudge checks an agent scorer as a task is checked, against the one
// agent that judges it and that agent's sandbox.
func (c *Config) validateJudge(add func(string, ...any), s Scorer) {
	what := fmt.Sprintf("scorer %q", s.Name)
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err != nil || d < 0 {
			add("%s: bad timeout %q (use e.g. 5m, 300s)", what, s.Timeout)
		}
	}
	checkEnv(add, what, s.Env)
	if s.Egress.Set {
		if _, err := (proxy.Policy{Allow: s.Egress.Allow, Deny: s.Egress.Deny}).Rule(); err != nil {
			add("%s: egress: %v", what, err)
		}
	}
	name, err := c.One("agent", s.Agent)
	if err != nil {
		add("%s: agent: %v", what, err)
		return
	}
	// one scorer, one judge: a panel of judges is several scorers.
	if name != s.Agent {
		add("%s: agent %q is a vector -- name the one agent that judges (%s)", what, s.Agent, name)
		return
	}
	var a Agent
	for _, x := range c.Agents {
		if x.Name == name {
			a = x
		}
	}
	sbID, err := c.ResolveSandbox(a)
	if err != nil {
		return // reported by validateAgents
	}
	var sb Sandbox
	for _, x := range c.Sandboxes {
		if x.Name == sbID {
			sb = x
		}
	}
	if s.Sudo != nil && *s.Sudo && sb.Driver != "lxd" {
		add("%s sets sudo, but its agent %q runs in sandbox %q (driver %q); sudo is lxd-only for now", what, name, sbID, sb.Driver)
	}
	if !s.Egress.Set {
		return
	}
	if !sb.Egress.Set {
		add("%s sets egress, but its agent %q runs in sandbox %q, whose egress is %s -- give the sandbox an egress mapping", what, name, sbID, sb.Egress.Mode())
		return
	}
	if sb.Egress.Default == "allow" {
		for _, line := range s.Egress.Allow {
			if e, err := proxy.ParseEntry(line, false); err == nil && !e.URL() {
				add("%s: egress allow %q is dead under sandbox %q's default: allow", what, line, sbID)
			}
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
			add("suite %q: agents is required", s.Name)
		}
		if len(s.Tasks) == 0 {
			add("suite %q: tasks is required", s.Name)
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
		for _, name := range agentSets[sc.Agent] {
			judges[name] = true
		}
	}
	for _, a := range c.Agents {
		if !inAgents[a.Name] && !judges[a.Name] {
			add("agent %q is in no suite -- it would never run (add it to a suite, or reference it from a scorer's agent)", a.Name)
		}
	}
	for _, t := range c.Tasks {
		if !inTasks[t.Name] {
			add("task %q is in no suite -- it would never run", t.Name)
		}
	}
	for _, s := range c.Scorers {
		if !inScorers[s.Name] {
			add("scorer %q is in no suite -- it would never run", s.Name)
		}
	}
}

// mark records every item name an axis reaches through sets.
func mark(m map[string]bool, xs StrList, sets map[string][]string) {
	for _, x := range xs {
		for _, name := range sets[x] {
			m[name] = true
		}
	}
}

// CheckCost says whether a cap in us dollars is one: finite, above zero, at
// most a million.
func CheckCost(c float64) error {
	if math.IsNaN(c) || math.IsInf(c, 0) || c <= 0 || c > 1e6 {
		return fmt.Errorf("%v is not a cost cap (a number above 0, at most 1e6)", c)
	}
	return nil
}
