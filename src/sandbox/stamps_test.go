package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// each cli's --version line, as the clis print them, reduces to its version;
// a cli the image lacks has no stamp.
func TestParseStamps(t *testing.T) {
	out := "claude=2.1.319 (Claude Code)\ncodex=codex-cli 0.167.0\ncopilot=GitHub Copilot CLI 1.0.107.\n"
	got, err := parseStamps(out)
	require.NoError(t, err)
	assert.EqualMaps(t, got, map[string]string{"claude": "2.1.319", "codex": "0.167.0", "copilot": "1.0.107"})
	got, err = parseStamps("x=1.0.0-beta.4 build\n")
	require.NoError(t, err)
	assert.EqualMaps(t, got, map[string]string{"x": "1.0.0-beta.4"})
	_, err = parseStamps("codex=error while loading shared libraries\n")
	assert.Error(t, err, "codex --version said")
}

// the script itself: a cli the image lacks has no line, one that cannot say
// its version stops it with 3.
func TestStampScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	write("claude", "echo '2.1.319 (Claude Code)'")
	write("codex", "echo 'codex-cli 0.167.0'")
	run := func() (string, error) {
		cmd := exec.Command(sh, "-c", stampScript)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run()
	require.NoError(t, err)
	got, err := parseStamps(out)
	require.NoError(t, err)
	assert.EqualMaps(t, got, map[string]string{"claude": "2.1.319", "codex": "0.167.0"})

	write("codex", "echo 'cannot load' >&2; exit 1")
	_, err = run()
	var ee *exec.ExitError
	require.That(t, errors.As(err, &ee), "a failing --version did not fail the script: %v", err)
	assert.Equal(t, ee.ExitCode(), 3)
}

// a provisioned lxd image is published with the harness versions inside it
// as properties, which the driver's Stamps reads back.
func TestLXDImageStampsOnServer(t *testing.T) {
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript {
			io.WriteString(stdout, "claude=2.1.319 (Claude Code)\n")
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	alias, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}, io.Discard)
	require.NoError(t, err)
	fp, err := l.fingerprint(context.Background(), alias)
	require.NoError(t, err)
	stamps, err := l.imageStamps(context.Background(), fp)
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"claude": "2.1.319"})
}

// a stamp container that fails or hangs is removed by name; an image with no
// shell has no stamps, any other failure is an error.
func TestContainerStampsFailures(t *testing.T) {
	type call = []string
	run := func(t *testing.T, fail func(args []string) error) ([]call, map[string]string, error) {
		var calls []call
		r := runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			calls = append(calls, args)
			if args[0] == "run" {
				return fail(args)
			}
			return nil
		})
		c := newContainer(testProcess(t), r, Options{Driver: "docker"})
		s, err := c.stamps(context.Background(), "img", time.Second)
		return calls, s, err
	}
	t.Run("no shell", func(t *testing.T) {
		calls, s, err := run(t, func([]string) error { return exitStatus(127) })
		require.NoError(t, err)
		assert.Len(t, s, 0)
		assert.Equal(t, calls[1][0], "rm")
	})
	t.Run("a runtime failure", func(t *testing.T) {
		calls, _, err := run(t, func([]string) error { return exitStatus(125) })
		assert.Error(t, err, "stamping img")
		assert.Equal(t, strings.Join(calls[1], " "), "rm -f "+calls[0][3])
	})
	t.Run("a hang", func(t *testing.T) {
		calls, _, err := run(t, func([]string) error { time.Sleep(1500 * time.Millisecond); return exitStatus(127) })
		assert.Error(t, err, "stamping img") // a timeout is never read as "no shell"
		assert.Equal(t, strings.Join(calls[1], " "), "rm -f "+calls[0][3])
	})
}

// an lxd stamp that hangs fails the build at its timeout, and the builder
// goes with it.
func TestLXDStampTimeoutOnServer(t *testing.T) {
	old := stampTimeout
	stampTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stampTimeout = old })
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript {
			<-ctx.Done()
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	_, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}, io.Discard)
	assert.Error(t, err, "stamping the harness versions took over")
	assert.Len(t, f.insts, 0) // the builder went with the failed build
}

// an image pats did not build has no stamp properties: its stamps come from
// a throwaway instance of it, which goes after.
func TestLXDStampsOfAForeignImageOnServer(t *testing.T) {
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript {
			io.WriteString(stdout, "codex=codex-cli 0.100.0\n")
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	fp, _, err := l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	stamps, err := l.imageStamps(context.Background(), fp)
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"codex": "0.100.0"})
	assert.Len(t, f.insts, 0)
}
