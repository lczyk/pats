package sandbox

import (
	"context"
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
	for _, driver := range []string{"docker", "podman"} {
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
