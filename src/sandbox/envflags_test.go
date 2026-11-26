package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a value never goes on the cli's command line: the flag names the key and
// the cli's own environment holds the value; a key the cli itself reads
// keeps KEY=value, as it would change the cli.
func TestEnvFlags(t *testing.T) {
	r := cliRunner("docker")
	flags, got := envFlags(r, "-e", map[string]string{"OPENROUTER_API_KEY": "sk-secret", "HOME": "/pats-home", "HTTPS_PROXY": "http://p:8080"})
	assert.EqualArrays(t, flags, []string{"-e", "HOME=/pats-home", "-e", "HTTPS_PROXY=http://p:8080", "-e", "OPENROUTER_API_KEY"})
	assert.That(t, !strings.Contains(strings.Join(flags, " "), "sk-secret"), "the secret is on the command line")
	assert.EqualArrays(t, got.(execRunner).env, []string{"OPENROUTER_API_KEY=sk-secret"})
	assert.EqualArrays(t, r.(execRunner).env, nil) // the shared runner is untouched

	// a fake has no environment to read: it keeps KEY=value.
	fake := runFunc(func(context.Context, io.Reader, io.Writer, io.Writer, ...string) error { return nil })
	flags, _ = envFlags(fake, "-e", map[string]string{"K": "v"})
	assert.EqualArrays(t, flags, []string{"-e", "K=v"})
}

// on a real docker, the value named by -e alone reaches the container.
func TestEnvFlagsOnDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil || exec.Command("docker", "info").Run() != nil {
		t.Skip("docker not available")
	}
	flags, r := envFlags(cliRunner("docker"), "-e", map[string]string{"PATS_TEST_SECRET": "s3cr'et"})
	out, err := output(context.Background(), r, append(append([]string{"run", "--rm"}, flags...), "docker.io/library/"+testImage, "sh", "-c", "printf %s \"$PATS_TEST_SECRET\"")...)
	require.NoError(t, err)
	assert.Equal(t, string(out), "s3cr'et")
}

// the lxd env script round-trips any value through a real shell.
func TestLXDEnvScriptShell(t *testing.T) {
	in := envScript(map[string]string{"A": "it's\n$x `y` \"z\"", "B": ""})
	cmd := exec.Command("/bin/sh", envArgv([]string{"sh", "-c", `printf '%s|%s' "$A" "$B"`})[1:]...)
	cmd.Stdin = in
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, string(out), "it's\n$x `y` \"z\"|")
}

// on a real docker and podman, a session's exec gets its env from stdin:
// the value reaches the command, never the cli's command line or its
// environment, and the command's own stdin is closed.
func TestExecEnvOnDaemon(t *testing.T) {
	for _, driver := range Engines() {
		t.Run(driver, func(t *testing.T) {
			c := onDaemon(t, driver)
			sb, err := c.Sandbox(SandboxOptions{Image: "docker.io/library/" + testImage})
			require.NoError(t, err)
			sess, err := sb.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			require.NoError(t, err)
			defer sess.Close()
			var out strings.Builder
			code, err := sess.Exec(context.Background(), Cmd{Argv: []string{"sh", "-c", `printf '%s|' "$SECRET"; cat`},
				Env: map[string]string{"SECRET": "s3cr'et\nline2"}}, &out, &out)
			require.NoError(t, err)
			assert.Equal(t, code, 0)
			assert.Equal(t, out.String(), "s3cr'et\nline2|")
		})
	}
}

// an env script that does not apply stops the command, which never runs
// without its env; the wrapper needs no cat.
func TestEnvArgvFailsClosed(t *testing.T) {
	cmd := exec.Command("/bin/sh", envArgv([]string{"sh", "-c", "echo ran"})[1:]...)
	cmd.Stdin = strings.NewReader("export A='unterminated\n")
	cmd.Env = []string{"PATH=/nonexistent"} // no cat, no sh on PATH: only the shell's own builtins
	out, err := cmd.CombinedOutput()
	require.That(t, err != nil, "the wrapper exited 0: %s", out)
	assert.That(t, !strings.Contains(string(out), "ran"), "the command ran: %s", out)
}

// an env script that arrives empty or cut short, as lxc exec can deliver
// it, stops the command with envLostCode before it runs, a cut inside a
// value included.
func TestEnvArgvRefusesShortScript(t *testing.T) {
	full, err := io.ReadAll(envScript(map[string]string{"A": "1", "B": "two\nlines", "PATS_PROMPT_FILE": "/p"}))
	require.NoError(t, err)
	for _, n := range []int{0, 1, 12, len(full) / 2, len(full) - 2} {
		cmd := exec.Command("/bin/sh", envArgv([]string{"sh", "-c", "echo ran"})[1:]...)
		cmd.Stdin = strings.NewReader(string(full[:n]))
		out, err := cmd.CombinedOutput()
		var x *exec.ExitError
		require.That(t, errors.As(err, &x), "cut at %d: the wrapper exited 0: %s", n, out)
		assert.Equal(t, x.ExitCode(), envLostCode)
		assert.That(t, envLost(x.ExitCode(), out), "cut at %d: no lost marker: %s", n, out)
		assert.That(t, !strings.Contains(string(out), "ran"), "cut at %d: the command ran: %s", n, out)
	}
	// the whole script without its last newline still applies.
	cmd := exec.Command("/bin/sh", envArgv([]string{"sh", "-c", `printf %s "$A"`})[1:]...)
	cmd.Stdin = strings.NewReader(strings.TrimSuffix(string(full), "\n"))
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, string(out), "1")
}

// an exec whose env arrives short runs again; one that keeps arriving
// short is a launch error, not an exit code. the command's own exit with
// the same code is its own.
func TestRetryEnvLost(t *testing.T) {
	lost := func(short int) func(io.Writer) (int, error) {
		n := 0
		return func(w io.Writer) (int, error) {
			n++
			if n <= short {
				_, _ = io.WriteString(w, envLostMsg+"\n")
				return envLostCode, nil
			}
			return 0, nil
		}
	}
	var errb strings.Builder
	code, err := retryEnvLost(&errb, lost(envTries-1))
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.That(t, strings.Contains(errb.String(), envLostMsg), "the lost line did not reach stderr")

	_, err = retryEnvLost(io.Discard, lost(envTries))
	assert.That(t, errors.Is(err, ErrLaunch), "want ErrLaunch, got %v", err)

	tries := 0
	code, err = retryEnvLost(io.Discard, func(io.Writer) (int, error) { tries++; return envLostCode, nil })
	require.NoError(t, err)
	assert.Equal(t, code, envLostCode)
	assert.Equal(t, tries, 1)
}
