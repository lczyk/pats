package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths are the paths pats names to an agent: its workdir (and cwd), its
// HOME, and where prepare's material lives while prepare runs.
type Paths struct {
	Work, Home, Prepare string
}

// the in-sandbox paths every driver but host binds or pushes to.
const (
	HomeMount    = "/pats-home"
	PrepareMount = "/pats-prepare"
)

// PathsOf are the paths sb's sessions tell an agent whose host workdir and
// home are these: the constants, but under host the dirs themselves.
func PathsOf(sb Sandbox, workdir, home string) Paths {
	if h, ok := sb.(interface{ paths(string, string) Paths }); ok {
		return h.paths(workdir, home)
	}
	return Paths{Work: WorkMount, Home: HomeMount, Prepare: PrepareMount}
}

// hostPassed are the names an agent under host takes from pats's own env:
// what a cli needs to be found and to talk (PATH, the locale, the terminal,
// the temp dir). nothing else is inherited.
var hostPassed = map[string]bool{"LANG": true, "TERM": true, "TMPDIR": true}

// hostEnv is the environment a host command runs with: the passed names from
// pats's env, PATH without relative or empty entries (so the cli preflight
// finds is the one every pair runs), then env over them, one entry a name.
func hostEnv(env map[string]string) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH":
			var keep []string
			for _, d := range filepath.SplitList(v) {
				if d != "" && filepath.IsAbs(d) {
					keep = append(keep, d)
				}
			}
			m["PATH"] = strings.Join(keep, string(os.PathListSeparator))
		case hostPassed[k], strings.HasPrefix(k, "LC_"):
			m[k] = v
		}
	}
	return sortedEnv(mergeEnv(m, env))
}

// lookIn finds name on path as exec.LookPath would on that PATH, not
// pats's own; a name with a slash is taken as it is.
func lookIn(path, name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on the agent's PATH", name)
}

// envValue is key's value in an environ list.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// HostLookPath finds a cli as a host agent would: on pats's PATH less its
// relative and empty entries.
func HostLookPath(name string) (string, error) {
	return lookIn(envValue(hostEnv(nil), "PATH"), name)
}
