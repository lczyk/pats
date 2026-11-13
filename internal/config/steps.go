package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/lczyk/pats/src/sandbox"
)

// hostSteps are an item's steps that run on the host, or in its step
// sandbox: a task's (and an agent scorer's) stage, prompt-cmd and collect,
// a script scorer's score.
func hostStepNames(stage, promptCmd, collect, score string) []string {
	var out []string
	for _, s := range []struct{ name, script string }{
		{"stage", stage}, {"prompt-cmd", promptCmd}, {"collect", collect}, {"score", score},
	} {
		if s.script != "" {
			out = append(out, s.name)
		}
	}
	return out
}

// hostSteps says whether this build runs a step on the host: one with an
// unboxed driver does.
var hostSteps = func() bool {
	return slices.ContainsFunc(sandbox.Drivers(), func(d string) bool { return sandbox.TraitsOf(d).Unboxed })
}

// AllowHostStepsInTests lets the host-step fixtures most tests are written
// in load in a build without host steps. tests only: a nohost build's
// TestMain calls it, and the tests of the rule itself put it back.
func AllowHostStepsInTests() (restore func()) {
	prev := hostSteps
	hostSteps = func() bool { return true }
	return func() { hostSteps = prev }
}

// validateSteps checks where host steps run (docs/spec/step-sandbox.md): a
// step-sandbox that resolves and is no host, step-env only beside one, and in
// a build with no host driver, no host step without one.
func (c *Config) validateSteps(add func(string, ...any)) {
	noHost := !hostSteps()
	steps := map[string]bool{} // sandboxes a step-sandbox names
	check := func(what string, stepSandbox string, stepEnv []string, env map[string]string, hs []string) {
		if stepSandbox == "" {
			if len(stepEnv) > 0 {
				add("%s: step-env needs a step-sandbox: a host step has the whole host environment", what)
			}
			if noHost {
				for _, h := range hs {
					add("%s: %s needs a step-sandbox: this build runs no host steps", what, h)
				}
			}
			return
		}
		name, err := c.One("sandbox", stepSandbox)
		if err != nil {
			add("%s: step-sandbox: %v", what, err)
		} else {
			steps[name] = true
			for _, s := range c.Sandboxes {
				if s.Name == name && sandbox.TraitsOf(s.Driver).Unboxed {
					add("%s: step-sandbox %q is a host sandbox: leave step-sandbox unset to run steps on the host", what, name)
				}
			}
		}
		if _, ok := env["HOME"]; ok {
			add("%s: env HOME: a step sandbox sets HOME itself", what)
		}
		for _, k := range stepEnv {
			switch {
			case !envName.MatchString(k):
				add("%s: step-env %q is not a valid variable name", what, k)
			case strings.HasPrefix(k, "PATS_"):
				add("%s: step-env %q: PATS_* is reserved for pats", what, k)
			case k == "PATH" || k == "HOME":
				add("%s: step-env %s is set by pats in the box", what, k)
			case shellReadonly[k]:
				add("%s: step-env %s is read-only in a shell", what, k)
			}
			if _, ok := env[k]; ok {
				add("%s: %s is in both env and step-env: set it in one", what, k)
			}
		}
	}
	for _, t := range c.Tasks {
		check(fmt.Sprintf("task %q", t.Name), t.StepSandbox, t.StepEnv, t.Env, hostStepNames(t.Stage, t.PromptCmd, t.Collect, ""))
	}
	for _, s := range c.Scorers {
		check(fmt.Sprintf("scorer %q", s.Name), s.StepSandbox, s.StepEnv, s.Env, hostStepNames(s.Stage, s.PromptCmd, s.Collect, s.Score))
	}
	// max-parallel bounds a sandbox's agents; on one only steps name it
	// would bound nothing.
	agents := map[string]bool{}
	for _, a := range c.Agents {
		if name, err := c.ResolveSandbox(a); err == nil {
			agents[name] = true
		}
	}
	names := make([]string, 0, len(steps))
	for n := range steps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, s := range c.Sandboxes {
			if s.Name == n && !agents[n] && s.MaxParallel > 0 {
				add("sandbox %q: max-parallel bounds agents, and only step-sandbox names this one", n)
			}
		}
	}
}
