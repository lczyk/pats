// Package agent turns an agent definition into a sandbox execution: the
// command, the environment and the credentials its harness entry names. the
// sandbox package handles isolation.
package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
)

// Spec builds the sandboxed execution for a task-running agent: its harness's
// one-shot command, which reads the model and the prompt from the PATS_* env
// and edits files in the cwd.
func Spec(h *machine.Harness, a config.Agent, workdir string, env map[string]string) (sandbox.Spec, error) {
	if h == nil || h.Run == "" {
		return sandbox.Spec{}, fmt.Errorf("agent %q: unsupported harness %q", a.Name, a.Harness)
	}
	return sandbox.Spec{Argv: []string{"sh", "-c", h.Run}, Workdir: workdir, Env: env}, nil
}

// Env assembles the PATS_* environment handed to a task-running agent.
// promptFile + outputDir are in-sandbox paths (see sandbox.WorkMount). runID
// is the run dir's basename, or "" outside a run (preflight).
func Env(a config.Agent, promptFile, outputDir, runID string) map[string]string {
	env := map[string]string{
		"PATS_MODEL":         a.Model,
		"PATS_AGENT_HARNESS": a.Harness,
		"PATS_PROMPT_FILE":   promptFile,
		"PATS_OUTPUT_DIR":    outputDir,
		"PATS_EFFORT":        a.Effort,
	}
	if runID != "" {
		env["PATS_RUN_NAME"] = runID
	}
	return env
}

// HomeEnv is the env a harness's cli derives from the in-sandbox HOME, or
// nil: a cli that resolves its state dir from a variable of its own rather
// than from $HOME is pointed at the dir the run phase copies its login into.
func HomeEnv(h *machine.Harness, home string) map[string]string {
	if h == nil || h.Cli() == nil || len(h.Cli().HomeEnv) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, rel := range h.Cli().HomeEnv {
		out[k] = filepath.Join(home, filepath.FromSlash(rel))
	}
	return out
}

// CredEnv returns the env vars the harness may inherit from the host, and
// whether any credential (an auth-env variable, not a pass-env one) was
// found. the agent runs as an arbitrary uid with no keychain access, so a
// token in the env is one way in; the other is the login file (HostCredsFile).
//
// the lists are per harness: forwarding every key to every harness lets an
// unrelated credential sitting in the host env change a harness's auth mode
// behind your back, which for a keyless one means it silently stops being
// keyless.
func CredEnv(h *machine.Harness) (env map[string]string, hasToken bool) {
	env = map[string]string{}
	if h == nil {
		return env, false
	}
	for _, k := range h.AuthEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
			hasToken = true
		}
	}
	for _, k := range h.PassEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	return env, hasToken
}

// AuthPathVar names the variable that points a harness at a copy of its
// login file, "" for a harness that copies none.
func AuthPathVar(h *machine.Harness) string {
	if h == nil || h.LoginFile() == "" {
		return ""
	}
	return h.Cli().AuthPathVar()
}

// HostCredsFile returns a harness's login file and its HOME-relative
// destination: what AuthPathVar names, else the cli's own file under the
// user's real home. a harness that copies no login returns empty strings, as
// does a default file that is not there. a named file that cannot be read, or
// a default one this process may not read (a strict snap and a home's
// dotfiles), is an error naming the variable.
func HostCredsFile(h *machine.Harness) (host, relative string, err error) {
	if h == nil || h.LoginFile() == "" {
		return "", "", nil
	}
	relative = filepath.FromSlash(h.LoginFile())
	v := AuthPathVar(h)
	if p := os.Getenv(v); p != "" {
		if _, err := os.ReadFile(p); err != nil {
			return "", "", fmt.Errorf("%s=%s: %w", v, p, err)
		}
		return p, relative, nil
	}
	home := os.Getenv("SNAP_REAL_HOME")
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", "", nil
		}
	}
	p := filepath.Join(home, relative)
	_, err = os.ReadFile(p)
	switch {
	case err == nil:
		return p, relative, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", "", nil
	}
	return "", "", fmt.Errorf("%s cannot be read here (%w): set %s to a copy of it", p, err, v)
}

// CredentialHint describes the host credential source used in auth warnings:
// the entry's own hint, else its variables and its login file.
func CredentialHint(h *machine.Harness) string {
	if h == nil {
		return "harness credentials"
	}
	if h.AuthHint != "" {
		return h.AuthHint
	}
	parts := append([]string(nil), h.AuthEnv...)
	if f := h.LoginFile(); f != "" {
		parts = append(parts, "~/"+f+" (or "+AuthPathVar(h)+")")
	}
	if len(parts) == 0 {
		return "harness credentials"
	}
	return strings.Join(parts, " or ")
}
