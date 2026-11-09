package sandbox

import (
	"io"
	"sort"
	"strings"
)

// a value on a cli's command line is readable by any local process (ps), so
// pats never puts a credential there. a command's env goes in on stdin as a
// script a shell reads first (envScript, envArgv), for docker, podman and
// lxc exec alike. the egress sidecar, started detached with no stdin, takes
// its env as `-e KEY` with the value in the cli's environment (envFlags):
// its keys are pats's own, and none is one the cli reads.

// cliReads is whether the docker or podman cli itself reads key: such a key
// cannot ride in the cli's environment without changing the cli (its config
// dir, its proxy, its tls roots), so it stays KEY=value on the command line.
// pats only sets such keys to its own non-secret values.
func cliReads(key string) bool {
	switch strings.ToUpper(key) {
	case "HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "SHELL", "TERM", "TZ", "LANG", "REGISTRY_AUTH_FILE",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
		return true
	}
	for _, p := range []string{"DOCKER_", "PODMAN_", "CONTAINERS_", "CONTAINER_", "BUILDAH_", "XDG_", "SSL_", "LC_"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// envFlags is the flag (-e) list for env, and the runner that carries the
// values: a cli runner takes them in its own environment and the flags name
// the keys alone. a runner that is not the cli (a test's fake) gets
// KEY=value flags, as it has no environment to read.
func envFlags(r runner, flag string, env map[string]string) ([]string, runner) {
	er, ok := r.(execRunner)
	var args []string
	for _, kv := range sortedEnv(env) {
		k, _, _ := strings.Cut(kv, "=")
		if ok && !cliReads(k) {
			args = append(args, flag, k)
			er.env = append(er.env, kv)
			continue
		}
		args = append(args, flag, kv)
	}
	if ok {
		return args, er
	}
	return args, r
}

// shQuote quotes s for a posix shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envScript is env as shell exports, read on stdin by envArgv's wrapper:
// values of any content, never on a command line.
func envScript(env map[string]string) io.Reader {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + shQuote(env[k]) + "\n")
	}
	return strings.NewReader(b.String())
}

// envArgv wraps argv to read its env from stdin first, then run with
// stdin closed (/dev/null), as an exec with no stdin had it. the script is
// read with the shell's own read, so the image needs no cat, and a script
// that does not apply stops the command rather than running it without.
func envArgv(argv []string) []string {
	const read = `s=; while IFS= read -r l || [ -n "$l" ]; do s="$s$l
"; done; eval "$s" || exit 126; exec "$@" </dev/null`
	return append([]string{"/bin/sh", "-c", read, "pats-env"}, argv...)
}
