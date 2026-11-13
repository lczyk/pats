package eval

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// setupTimeout bounds bringing a pair's sandbox up and preparing it: start,
// push, the in-sandbox prepare. the task's timeout covers the agent only.
const setupTimeout = 10 * time.Minute

// syncTimeout bounds bringing the agent's files home after it ran.
const syncTimeout = 10 * time.Minute

// prepareMount is where the prepare step's material lives inside the sandbox
// while it runs: the script and the pats root's bin/. it is
// removed before the agent starts, since the agent can read everything else.
// under the host driver it is a dir in the harness home (sandbox.PathsOf).
const prepareMount = sandbox.PrepareMount

// prepareEnv is the env an in-sandbox prepare runs with: the task's own, and
// PATS_* naming in-sandbox paths. no PATS_OUTPUT_DIR or PATS_TASK_DIR -- those
// are host paths, and anything secret stays in stage.
func prepareEnv(job pairJob, runID string, paths sandbox.Paths) map[string]string {
	env := map[string]string{
		"PATS_AGENT_NAME":    job.agentName,
		"PATS_AGENT_HARNESS": job.agent.Harness,
		"PATS_MODEL":         job.agent.Model,
		"PATS_RUN_NAME":      runID,
		"PATS_WORKDIR":       paths.Work,
		"PATS_AGENT_HOME":    paths.Home,
	}
	maps.Copy(env, job.names)
	maps.Copy(env, job.task.Env)
	return env
}

// stageMaterial stages a step that runs inside a sandbox or a builder: the
// pats root's bin/ and the script, in a fresh temp dir; argv runs it once the
// dir is at mount (by its #! line, else under /bin/sh -e). the caller removes
// dir. driver is the sandbox's: its cli may be the one that reads the dir.
func stageMaterial(driver, root, script, mount string) (material string, argv []string, err error) {
	material, err = sandbox.MkTempFor(driver, "pats-material-")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			sandbox.RemoveTemp(material)
		}
	}()
	if err := copyTree(filepath.Join(root, binDir), filepath.Join(material, "bin")); err != nil {
		return "", nil, err
	}
	mode := os.FileMode(0o644)
	if strings.HasPrefix(script, "#!") {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(material, "script"), []byte(script), mode); err != nil {
		return "", nil, err
	}
	if mode == 0o755 {
		return material, []string{mount + "/script"}, nil
	}
	return material, []string{"/bin/sh", "-e", mount + "/script"}, nil
}

// inPath wraps argv so mount/bin is first on the sandbox's PATH, which is not
// known from the host. mount is an argument, never shell source: under the
// host driver it is a host path, which may hold anything a dir name may.
func inPath(mount string, argv []string) []string {
	return append([]string{"/bin/sh", "-c", `PATH="$1/bin:$PATH"; export PATH; shift; exec "$@"`, "step", mount}, argv...)
}

// runPrepare runs the task's prepare inside the started sandbox as root: its
// material is pushed to mount, run with mount/bin first on PATH and the
// workdir as cwd, then removed. output goes to log.
func runPrepare(ctx context.Context, sess sandbox.Session, driver, root, taskDir string, t config.Task, env map[string]string, prepareMount string, log io.Writer) error {
	material, argv, err := stageMaterial(driver, root, t.Prepare, prepareMount)
	if err != nil {
		return err
	}
	defer sandbox.RemoveTemp(material)
	if err := sess.Push(ctx, material, prepareMount); err != nil {
		return err
	}
	code, err := sess.Exec(ctx, sandbox.Cmd{Argv: inPath(prepareMount, argv), Env: env, Root: true}, log, log)
	// the agent must never see this material: a failed removal fails setup.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	ccode, cerr := sess.Exec(cctx, sandbox.Cmd{Argv: []string{"rm", "-rf", prepareMount}, Root: true}, io.Discard, io.Discard)
	if cerr == nil && ccode != 0 {
		cerr = fmt.Errorf("exited %d", ccode)
	}
	if cerr != nil && err == nil {
		err = fmt.Errorf("remove %s: %w", prepareMount, cerr)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exited %d (see prepare.log)", code)
	}
	return nil
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
