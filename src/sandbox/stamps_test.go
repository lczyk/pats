package sandbox

import (
	"archive/tar"
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
		cmd := exec.Command(sh, "-c", stampScript(allCLIs))
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run()
	require.NoError(t, err)
	got, err := parseStamps(out)
	require.NoError(t, err)
	assert.EqualMaps(t, got, map[string]string{"claude": "2.1.319", "codex": "0.167.0"})

	// only the clis asked for are run, and with none nothing is.
	only := exec.Command(sh, "-c", stampScript([]string{"codex", "not a cli; rm -rf /"}))
	only.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	b, err := only.Output()
	require.NoError(t, err)
	assert.Equal(t, string(b), "codex=codex-cli 0.167.0\n")
	assert.Equal(t, stampScript(nil), ":")

	// a cli that says something else first: the line is the first that
	// holds a version, and the first of all when none does.
	write("copilot", "echo 'Package extraction took 5417ms'; echo 'GitHub Copilot CLI 1.0.107.'; echo 'Run copilot update to check'")
	write("opencode", "echo 'no version here'; echo 'nor here'")
	out, err = run()
	require.NoError(t, err)
	assert.ContainsString(t, out, "copilot=GitHub Copilot CLI 1.0.107.\n")
	assert.ContainsString(t, out, "opencode=no version here\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "opencode")))

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
	asked := 0
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && strings.HasPrefix(argv[2], "for c in ") {
			asked++
			if strings.Contains(argv[2], "claude") {
				io.WriteString(stdout, "claude=2.1.319 (Claude Code)\n")
			}
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	alias, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}, Stamp: []string{"claude"}}, io.Discard)
	require.NoError(t, err)
	fp, err := l.fingerprint(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, asked, 1) // the build asked
	stamps, err := l.imageStamps(context.Background(), fp, []string{"claude"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"claude": "2.1.319"})
	assert.Equal(t, asked, 1) // the properties answered

	// a cli the properties do not answer for was absent at the build or not
	// asked about: the image itself is asked.
	stamps, err = l.imageStamps(context.Background(), fp, []string{"claude", "codex"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"claude": "2.1.319"})
	assert.Equal(t, asked, 2)
	assert.Len(t, f.insts, 0) // the throwaway instance is gone

	none, err := l.imageStamps(context.Background(), fp, nil)
	require.NoError(t, err)
	assert.Len(t, none, 0)
	assert.Equal(t, asked, 2)
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
		s, err := c.stamps(context.Background(), "img", allCLIs, time.Second)
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
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript(allCLIs) {
			<-ctx.Done()
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	l.p.t.stamp = 200 * time.Millisecond
	_, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}, Stamp: allCLIs}, io.Discard)
	assert.Error(t, err, "stamping the harness versions took over")
	assert.Len(t, f.insts, 0) // the builder went with the failed build
}

// an image pats did not build has no stamp properties: its stamps come from
// a throwaway instance of it, which goes after.
func TestLXDStampsOfAForeignImageOnServer(t *testing.T) {
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript(allCLIs) {
			io.WriteString(stdout, "codex=codex-cli 0.100.0\n")
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	fp, _, err := l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	stamps, err := l.imageStamps(context.Background(), fp, allCLIs)
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"codex": "0.100.0"})
	assert.Len(t, f.insts, 0)
}

// allCLIs are the clis the stamp tests ask about.
var allCLIs = []string{"claude", "codex", "copilot", "opencode"}

// an lxd build places each cli after its own step: the tree is pushed to the
// builder and copied over its root as root's, then the image is stamped. the
// image's key moves with what decides a tree, and not with its url.
func TestLXDImagePlacesHarness(t *testing.T) {
	f := newFakeServer(t)
	var steps []string
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		switch {
		case len(argv) == 3 && strings.Contains(argv[2], "cp -a "+placeStage):
			steps = append(steps, "place")
			for _, want := range []string{"chown -R 0:0 /opt/pats/harness/tool", "chown -h 0:0 /usr/local/bin/tool", "rm -rf " + placeStage} {
				assert.ContainsString(t, argv[2], want)
			}
		case len(argv) == 3 && strings.HasPrefix(argv[2], "for c in tool;"):
			steps = append(steps, "stamp")
			io.WriteString(stdout, "tool=tool 1.2.3\n")
		case len(argv) > 0 && argv[len(argv)-1] == "tools-step":
			steps = append(steps, "argv")
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	file := tarGz(t, tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\necho 'tool 1.2.3'\n"})
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", URL: "https://a.example/t", SHA256: "aa", Bin: "package/tool"}
	spec := lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"tools-step"}, Stamp: []string{"tool"}, Harness: []Placed{{Artefact: a, File: file}}}
	alias, fp, _, built, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, built, true)
	assert.EqualArrays(t, steps, []string{"argv", "place", "stamp"})
	stamps, err := l.imageStamps(context.Background(), fp, []string{"tool"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"tool": "1.2.3"})

	key := func(change func(*Artefact)) string {
		b := a
		change(&b)
		k, err := imageKey("base-fp", lxdImageSpec{Argv: []string{"tools-step"}, Harness: []Placed{{Artefact: b}}}, false)
		require.NoError(t, err)
		return k
	}
	same := key(func(*Artefact) {})
	assert.Equal(t, key(func(x *Artefact) { x.URL = "https://mirror.example/t" }), same)
	assert.That(t, key(func(x *Artefact) { x.SHA256 = "bb" }) != same, "the key did not move with the sha")
	assert.That(t, key(func(x *Artefact) { x.Bin = "package/bin/tool" }) != same, "the key did not move with the bin")
	none, err := imageKey("base-fp", lxdImageSpec{Argv: []string{"tools-step"}}, false)
	require.NoError(t, err)
	assert.That(t, none != same, "the key did not move with a cli placed")
	assert.That(t, strings.HasPrefix(alias, "pats-img-"), alias)
}
