package eval

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// binDir is the directory under the pats root put on every host script's PATH,
// so shared helpers are called by name.
const binDir = "bin"

// step is one host-side script, run in dir: the directory of the file the item
// was defined in.
type step struct {
	script string
	dir    string
}

func (s step) set() bool { return s.script != "" }

// command builds the step's process with root/bin prepended to PATH, PATS_ROOT
// and env on top of the host environment. the script is written to a temp
// file: with a #! line it is exec'd (its shebang picks the interpreter),
// without one it runs under `/bin/sh -e`, so a one-liner like `./collect.sh
// --fast` runs that file. the returned cleanup removes the temp file.
func (s step) command(root string, env map[string]string) (*exec.Cmd, func(), error) {
	// PWD follows the dir the script runs in: the inherited one names pats's
	// own cwd, and a shell trusts it when it resolves to the same place.
	environ := append(hostEnviron(root, env), "PWD="+s.dir)
	f, err := os.CreateTemp("", "pats-script-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	_, werr := f.WriteString(s.script)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		cleanup()
		return nil, nil, werr
	}
	var cmd *exec.Cmd
	if strings.HasPrefix(s.script, "#!") {
		if err := os.Chmod(f.Name(), 0o700); err != nil {
			cleanup()
			return nil, nil, err
		}
		cmd = exec.Command(f.Name())
	} else {
		cmd = exec.Command("/bin/sh", "-e", f.Name())
	}
	cmd.Dir, cmd.Env = s.dir, environ
	return cmd, cleanup, nil
}

// run executes the step with stdout and stderr streamed to out.
func (s step) run(root string, env map[string]string, out io.Writer) error {
	cmd, cleanup, err := s.command(root, env)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// output executes the step and returns its stdout. a failure carries the tail
// of its stderr.
func (s step) output(root string, env map[string]string) ([]byte, error) {
	return s.outputTee(root, env, nil)
}

// outputTee is output, with stdout and stderr also written to tee when set.
func (s step) outputTee(root string, env map[string]string, tee io.Writer) ([]byte, error) {
	cmd, cleanup, err := s.command(root, env)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if tee != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, tee), io.MultiWriter(&stderr, tee)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrTail(&stderr))
	}
	return stdout.Bytes(), nil
}

// stepNames is every name pats sets in some host step's env. an inherited
// one is dropped before a step's own map goes on, so a pats started from
// inside another's step never hands the parent's values to a step whose map
// leaves the name out. pats's own settings (PATS_LXD_UPLINK and the like)
// are in no step map and pass through.
var stepNames = map[string]bool{
	"PATS_ROOT": true, "PATS_RUN_NAME": true, "PATS_RUN_DIR": true,
	"PATS_AGENT_NAME": true, "PATS_AGENT_HARNESS": true, "PATS_AGENT_HOME": true, "PATS_MODEL": true,
	"PATS_TASK_NAME": true, "PATS_TASK_VECTOR": true, "PATS_TASK_DIR": true,
	"PATS_WORKDIR": true, "PATS_OUTPUT_DIR": true,
	"PATS_SCORER_NAME": true, "PATS_SCORER_VECTOR": true,
	"PATS_SCORED_DIR": true, "PATS_SCORED_AGENT_NAME": true, "PATS_SCORED_TASK_NAME": true,
	"PATS_SCORED_TASK_DIR": true, "PATS_SCORED_MODEL": true, "PATS_SCORED_HARNESS": true,
}

// hostEnviron is the host environment for a script: os env less PWD and
// stepNames, with root/bin prepended to PATH (when it exists), PATS_ROOT,
// then env.
func hostEnviron(root string, env map[string]string) []string {
	path := os.Getenv("PATH")
	if fi, err := os.Stat(filepath.Join(root, binDir)); err == nil && fi.IsDir() {
		path = filepath.Join(root, binDir) + string(os.PathListSeparator) + path
	}
	out := make([]string, 0, len(os.Environ())+len(env)+2)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k != "PATH" && k != "PWD" && !stepNames[k] {
			out = append(out, kv)
		}
	}
	out = append(out, "PATH="+path, "PATS_ROOT="+root)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
