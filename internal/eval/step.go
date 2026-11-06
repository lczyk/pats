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

// step is one host-side script: an inline body or a command line (at most one
// is set), run in dir -- the directory of the file the item was defined in,
// which relative paths in cmd resolve against too.
type step struct {
	inline string
	cmd    string
	dir    string
}

func (s step) set() bool { return s.inline != "" || s.cmd != "" }

// command builds the step's process with root/bin prepended to PATH, PATS_ROOT
// and env on top of the host environment. an inline body is written to a
// temp file: with a #! line it is exec'd (its shebang picks the interpreter),
// without one it runs under `/bin/sh -e`. a cmd is split into words (posix
// quoting, no shell); a first word containing a / is a file resolved against
// dir, a bare one is looked up on PATH. the returned cleanup removes the temp
// file.
func (s step) command(root string, env map[string]string) (*exec.Cmd, func(), error) {
	// PWD follows the dir the script runs in: the inherited one names pats's
	// own cwd, and a shell trusts it when it resolves to the same place.
	environ := append(hostEnviron(root, env), "PWD="+s.dir)
	nop := func() {}
	if s.inline != "" {
		f, err := os.CreateTemp("", "pats-script-")
		if err != nil {
			return nil, nop, err
		}
		cleanup := func() { os.Remove(f.Name()) }
		_, werr := f.WriteString(s.inline)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			cleanup()
			return nil, nop, werr
		}
		var cmd *exec.Cmd
		if strings.HasPrefix(s.inline, "#!") {
			if err := os.Chmod(f.Name(), 0o700); err != nil {
				cleanup()
				return nil, nop, err
			}
			cmd = exec.Command(f.Name())
		} else {
			cmd = exec.Command("/bin/sh", "-e", f.Name())
		}
		cmd.Dir, cmd.Env = s.dir, environ
		return cmd, cleanup, nil
	}

	argv, err := splitArgs(s.cmd)
	if err != nil {
		return nil, nop, err
	}
	if len(argv) == 0 {
		return nil, nop, fmt.Errorf("empty command")
	}
	path, err := resolveCommand(argv[0], s.dir, pathOf(environ))
	if err != nil {
		return nil, nop, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir, cmd.Env = s.dir, environ
	return cmd, nop, nil
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
	cmd, cleanup, err := s.command(root, env)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrTail(&stderr))
	}
	return stdout.Bytes(), nil
}

// hostEnviron is the host environment for a script: os env with root/bin
// prepended to PATH (when it exists), PATS_ROOT, then env.
func hostEnviron(root string, env map[string]string) []string {
	path := os.Getenv("PATH")
	if fi, err := os.Stat(filepath.Join(root, binDir)); err == nil && fi.IsDir() {
		path = filepath.Join(root, binDir) + string(os.PathListSeparator) + path
	}
	out := make([]string, 0, len(os.Environ())+len(env)+2)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}
	out = append(out, "PATH="+path, "PATS_ROOT="+root)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// pathOf returns the PATH value from an environ list.
func pathOf(environ []string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], "PATH="); ok {
			return v
		}
	}
	return ""
}

// resolveCommand finds a command's executable: a name containing a / is a
// file resolved against dir, a bare name is searched for on path.
func resolveCommand(name, dir, path string) (string, error) {
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, checkExecutable(p)
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if checkExecutable(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found (not in bin/ or on PATH)", name)
}

func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable (chmod +x)", p)
	}
	return nil
}
