package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a read-only copy is root's alone to change: no write but the owner's, read
// for all, execute for all on a dir or where the owner has it, no setuid.
func TestROModes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		dir  bool
		want int64
	}{
		{0o644, false, 0o644},
		{0o666, false, 0o644},
		{0o600, false, 0o644},
		{0o700, false, 0o755},
		{0o4755, false, 0o755},
		{0o2775, true, 0o755},
		{0o700, true, 0o755},
	} {
		assert.Equal(t, roModes(c.in, c.dir), c.want, "mode %o dir %v", c.in, c.dir)
	}
}

// a masked path arrives as an empty dir whatever it held, and nothing
// under it travels.
func TestWriteTarMasks(t *testing.T) {
	src := t.TempDir()
	for _, p := range []string{"keep/a", ".pats/runs/x/out", "..cache/tmp/f"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, p), []byte("x"), 0o600))
	}
	fi, err := os.Stat(src)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, src, fi, tarOpts{ro: true, skip: []string{".pats", "..cache"}}))
	got := map[string]int64{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got[h.Name] = h.Mode
	}
	assert.Equal(t, len(got), 4, "entries: %v", got)
	for _, n := range []string{"keep/", "keep/a", ".pats/", "..cache/"} {
		_, ok := got[n]
		assert.That(t, ok, "no %s in %v", n, got)
	}
	assert.Equal(t, got["keep/a"], int64(0o644))
}

// an entry gone before it is opened is left out, not an error.
func TestWriteTarVanished(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a"), []byte("x"), 0o644))
	fi, err := os.Stat(filepath.Join(src, "a"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(src, "a")))
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, filepath.Join(src, "a"), fi, tarOpts{}))
	_, err = tar.NewReader(&buf).Next()
	assert.Equal(t, err, io.EOF)
}

// a file that grows while it is pushed goes as the prefix its open size
// gave; one that shrinks fails, as ErrChanged, for the caller to retry.
func TestWriteTarChangingFile(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*os.File) error
		err    error
	}{
		{"grows", func(f *os.File) error { _, err := f.WriteString("more"); return err }, nil},
		{"shrinks", func(f *os.File) error { return f.Truncate(1) }, ErrChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "log")
			require.NoError(t, os.WriteFile(p, []byte("hello"), 0o644))
			prev := pushOpened
			pushOpened = func(string) {
				f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				require.NoError(t, err)
				require.NoError(t, c.change(f))
				f.Close()
			}
			t.Cleanup(func() { pushOpened = prev })
			fi, err := os.Stat(p)
			require.NoError(t, err)
			var buf bytes.Buffer
			err = writeTar(&buf, p, fi, tarOpts{})
			if c.err != nil {
				assert.ErrorIs(t, err, c.err)
				return
			}
			require.NoError(t, err)
			tr := tar.NewReader(&buf)
			h, err := tr.Next()
			require.NoError(t, err)
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			assert.Equal(t, h.Size, int64(5))
			assert.Equal(t, string(body), "hello")
		})
	}
}

// binds are owned parents first, a file bind's parents are opened for the
// agent too, and a read-only dir is made root's whatever the image had
// there.
func TestLXDStartReadOnlyBinds(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "pats-img-x"})
	work, ref, file := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "cfg")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	sess, err := l.Start(context.Background(), Spec{Workdir: work, Mounts: []Mount{
		{Host: ref, Container: "/opt/ref", ReadOnly: true},
		{Host: t.TempDir(), Container: "/opt/ref/out"},
		{Host: file, Container: "/home/ubuntu/cfg"},
	}})
	require.NoError(t, err)
	defer sess.Close()
	var order []string
	for _, c := range f.all() {
		switch {
		case strings.Contains(c, "-- sh -c d=") && strings.Contains(c, "/home/ubuntu/cfg"):
			// the file's own dir is opened first, then those above it.
			assert.ContainsString(t, c, `do [ -d "$d" ] && chmod a+x "$d"; d=$(dirname "$d"); done`)
			order = append(order, "file push")
		case strings.Contains(c, "-- sh -c d=") && strings.Contains(c, " /opt/ref /opt/ref "):
			assert.ContainsString(t, c, `chown 0:0 "$1" && chmod "$5" "$1"`)
			order = append(order, "ro push")
		case strings.Contains(c, " owners "):
			// parents first, so a bind inside another wins over it.
			assert.ContainsString(t, c, "1001 "+WorkMount+" 0 /opt/ref 1001 /opt/ref/out 1001 /home/ubuntu/cfg")
			order = append(order, "owners")
		}
	}
	assert.EqualArrays(t, order, []string{"ro push", "file push", "owners"})
}

// a file pushed through a symlink goes in under its destination's name, not
// its source's, which could be a file the image has beside it.
func TestWriteTarFileName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	fi, err := os.Stat(p)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, p, fi, tarOpts{name: "config"}))
	h, err := tar.NewReader(&buf).Next()
	require.NoError(t, err)
	assert.Equal(t, h.Name, "config")
}

// a read-only bind under a writable one could be swapped out by the agent:
// refused at start.
func TestLXDStartReadOnlyUnderWritable(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "pats-img-x"})
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Mounts: []Mount{
		{Host: t.TempDir(), Container: WorkMount + "/ref", ReadOnly: true},
	}})
	assert.Error(t, err, "read-only /workspace/ref lies under writable /workspace")
	assert.NotNil(t, f.call("delete"), "the instance outlived the refusal")
}

// the push script, run by a real shell and tar, lands a whole tar, and
// refuses one cut short with pushLostCode, a cut at a block boundary (which
// tar takes for the whole archive) included.
func TestPushArgvCutTar(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a"), bytes.Repeat([]byte("a"), 15872), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "b"), []byte("b"), 0o644))
	fi, err := os.Stat(src)
	require.NoError(t, err)
	end, err := pushEndName()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, src, fi, tarOpts{end: end}))
	run := func(in []byte) (string, int) {
		dest := filepath.Join(t.TempDir(), "dest")
		argv := pushArgv(dest, end, fi, pushOpts{})
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = bytes.NewReader(in)
		out, err := cmd.CombinedOutput()
		var x *exec.ExitError
		if errors.As(err, &x) {
			return dest, x.ExitCode()
		}
		require.NoError(t, err, "%s", out)
		return dest, 0
	}
	dest, code := run(buf.Bytes())
	assert.Equal(t, code, 0)
	got, err := os.ReadFile(filepath.Join(dest, "b"))
	require.NoError(t, err)
	assert.Equal(t, string(got), "b")
	_, err = os.Stat(filepath.Join(dest, end))
	assert.That(t, os.IsNotExist(err), "the end marker stayed: %v", err)
	// 512 header + 15872 data: the cut lands on a block boundary after a.
	for _, n := range []int{0, 16384, buf.Len() - 1024 - 512} {
		_, code := run(buf.Bytes()[:n])
		assert.Equal(t, code, pushLostCode)
	}
}

// a push whose tar arrives short goes again, and one that keeps arriving
// short fails.
func TestLXDPushRetriesShortTar(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	src := filepath.Join(t.TempDir(), "creds")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o600))
	s := &lxdSession{l: l, name: "pats-x"}
	f.cutPushes = pushTries - 1
	require.NoError(t, s.Push(context.Background(), src, "/etc/creds"))
	f.cutPushes = pushTries
	err := s.Push(context.Background(), src, "/etc/creds")
	require.That(t, err != nil, "a push cut every time succeeded")
	assert.ContainsString(t, err.Error(), "arrived short")
}
