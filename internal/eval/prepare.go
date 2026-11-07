package eval

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// setupTimeout bounds bringing a pair's sandbox up and preparing it: start,
// push, the in-sandbox prepare. the task's timeout covers the agent only.
const setupTimeout = 10 * time.Minute

// prepareMount is where the prepare step's material lives inside the sandbox
// while it runs: the script, a -cmd's file, and the pats root's bin/. it is
// removed before the agent starts, since the agent can read everything else.
const prepareMount = "/pats-prepare"

// prepareEnv is the env an in-sandbox prepare runs with: the task's own, and
// PATS_* naming in-sandbox paths. no PATS_OUTPUT_DIR or PATS_TASK_DIR -- those
// are host paths, and anything secret stays in stage.
func prepareEnv(t config.Task, a config.Agent, agentID, runID string) map[string]string {
	env := map[string]string{
		"PATS_TASK_ID":       t.ID,
		"PATS_AGENT_ID":      agentID,
		"PATS_AGENT_HARNESS": a.Harness,
		"PATS_MODEL":         a.Model,
		"PATS_RUN_ID":        runID,
		"PATS_WORKDIR":       sandbox.WorkMount,
		"PATS_AGENT_HOME":    homeMount,
	}
	for k, v := range t.Env {
		env[k] = v
	}
	return env
}

// runPrepare runs the task's prepare inside the started sandbox as root: its
// material is pushed to prepareMount, run with prepareMount/bin first on PATH
// and the workdir as cwd, then removed, and what root left in the workdir and
// home is handed to the agent user. output goes to log.
func runPrepare(ctx context.Context, sess sandbox.Session, root, taskDir string, t config.Task, env map[string]string, log io.Writer) error {
	material, err := sandbox.MkTemp("pats-prepare-")
	if err != nil {
		return err
	}
	defer sandbox.RemoveTemp(material)

	if err := copyTree(filepath.Join(root, binDir), filepath.Join(material, "bin")); err != nil {
		return err
	}
	var argv []string
	switch {
	case t.Prepare != "":
		mode := os.FileMode(0o644)
		if strings.HasPrefix(t.Prepare, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(material, "script"), []byte(t.Prepare), mode); err != nil {
			return err
		}
		if mode == 0o755 {
			argv = []string{prepareMount + "/script"}
		} else {
			argv = []string{"/bin/sh", "-e", prepareMount + "/script"}
		}
	default:
		argv, err = splitArgs(t.PrepareCmd)
		if err != nil {
			return err
		}
		if len(argv) == 0 {
			return fmt.Errorf("empty command")
		}
		// a file is pushed in; a bare name is looked up on the sandbox's PATH,
		// bin/ first.
		if strings.Contains(argv[0], "/") {
			p, err := resolveCommand(argv[0], taskDir, "")
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(material, "cmd"), 0o755); err != nil {
				return err
			}
			dst := filepath.Join(material, "cmd", filepath.Base(p))
			if err := copyFile(p, dst); err != nil {
				return err
			}
			if err := os.Chmod(dst, 0o755); err != nil {
				return err
			}
			argv[0] = path.Join(prepareMount, "cmd", filepath.Base(p))
		}
	}
	if err := sess.Push(ctx, material, prepareMount); err != nil {
		return err
	}
	// the sandbox's own PATH is not known from here; the shell extends it.
	wrapped := append([]string{"/bin/sh", "-c", `PATH="` + prepareMount + `/bin:$PATH"; export PATH; exec "$@"`, "prepare"}, argv...)
	code, err := sess.Exec(ctx, sandbox.Cmd{Argv: wrapped, Env: env, Root: true}, log, log)
	cleanup := []string{"rm", "-rf", prepareMount}
	if _, cerr := sess.Exec(context.WithoutCancel(ctx), sandbox.Cmd{Argv: cleanup, Root: true}, io.Discard, io.Discard); cerr != nil && err == nil {
		err = fmt.Errorf("remove %s: %w", prepareMount, cerr)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exited %d (see prepare.log)", code)
	}
	return sess.HandOver(ctx, sandbox.WorkMount, homeMount)
}

// copyTree copies the regular files and dirs under src to dst, keeping their
// modes and following symlinks; a missing src copies nothing.
func copyTree(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, fi.Mode().Perm())
	})
}
