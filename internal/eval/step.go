package eval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
//
// the step runs in a process group of its own, killed whole when ctx ends: a
// hung step and whatever it started never outlive the run.
func (s step) command(ctx context.Context, root string, env map[string]string) (*exec.Cmd, func(), error) {
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
		cmd = exec.CommandContext(ctx, f.Name())
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-e", f.Name())
	}
	cmd.Dir, cmd.Env = s.dir, environ
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, cleanup, nil
}

// run executes the step with stdout and stderr streamed to out.
func (s step) run(ctx context.Context, root string, env map[string]string, out io.Writer) error {
	cmd, cleanup, err := s.command(ctx, root, env)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout, cmd.Stderr = out, out
	return runGroup(ctx, cmd)
}

// output executes the step and returns its stdout. a failure carries the tail
// of its stderr.
func (s step) output(ctx context.Context, root string, env map[string]string) ([]byte, error) {
	return s.outputTee(ctx, root, env, nil)
}

// outputTee is output, with stdout and stderr also written to tee when set.
func (s step) outputTee(ctx context.Context, root string, env map[string]string, tee io.Writer) ([]byte, error) {
	cmd, cleanup, err := s.command(ctx, root, env)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if tee != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, tee), io.MultiWriter(&stderr, tee)
	}
	if err := runGroup(ctx, cmd); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrTail(&stderr))
	}
	return stdout.Bytes(), nil
}

// runGroup runs cmd in its own process group, and a step's processes end
// with it: the group is killed when ctx ends while it runs, and when the
// script exits, so nothing it started in the background outlives it. the
// output goes through pipes runGroup owns, drained only after that kill: a
// child left holding the output cannot keep the step waiting.
func runGroup(ctx context.Context, cmd *exec.Cmd) error {
	var ours []*os.File // the child's ends, closed in this process once it has them
	var reads []*os.File
	var drains []chan struct{}
	piped := map[io.Writer]*os.File{}
	wire := func(dst io.Writer) (io.Writer, error) {
		if _, isFile := dst.(*os.File); isFile || dst == nil {
			return dst, nil
		}
		if w, ok := piped[dst]; ok { // stdout and stderr to one writer: one pipe
			return w, nil
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		done := make(chan struct{})
		go func() {
			io.Copy(dst, r)
			close(done)
		}()
		piped[dst], ours, reads, drains = w, append(ours, w), append(reads, r), append(drains, done)
		return w, nil
	}
	finish := func(waitDrains bool) {
		for _, w := range ours {
			w.Close()
		}
		if waitDrains {
			// the group is gone, so the pipes' other ends are closed and the
			// drains end. one that does not (a child that left the group) is
			// cut off.
			timeout := time.After(stepWaitDelay)
			for _, d := range drains {
				select {
				case <-d:
				case <-timeout:
				}
			}
		}
		for _, r := range reads {
			r.Close()
		}
	}
	var err error
	if cmd.Stdout, err = wire(cmd.Stdout); err == nil {
		cmd.Stderr, err = wire(cmd.Stderr)
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		finish(false)
		return err
	}
	for _, w := range ours {
		w.Close()
	}
	ours = nil
	pid := cmd.Process.Pid
	kill := func() { syscall.Kill(-pid, syscall.SIGKILL) }
	stop := context.AfterFunc(ctx, kill)
	defer stop()
	err = cmd.Wait()
	kill()
	finish(true)
	return err
}

// stepWaitDelay bounds the wait for a finished step's output to drain,
// should something outside its group still hold the pipe.
const stepWaitDelay = 10 * time.Second

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
