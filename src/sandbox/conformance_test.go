package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the session conformance table: what Session promises, run against every
// driver's fake. a new driver joins the table and passes before it ships
// (docs/spec/sandbox-seams.md).

// conformDriver is one driver on its fake: a started session, and the
// fake's view of what runs and what is left.
type conformDriver struct {
	name  string
	start func(t *testing.T, spec Spec) (Session, *conformFake)
}

type conformFake struct {
	id    string // the sandbox, as the fake names it
	agent func(fn func(ctx context.Context, argv []string, stdout, stderr io.Writer) error)
	write func(p, content string)
	read  func(p string) (string, bool)
	left  func() []string
}

var conformDrivers = []conformDriver{
	{"lxd", func(t *testing.T, spec Spec) (Session, *conformFake) {
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{ProxyBin: proxyBin(t)})
		sess, err := l.Start(context.Background(), spec)
		require.NoError(t, err)
		id := sess.(*lxdSession).name
		return sess, &conformFake{id: id,
			agent: func(fn func(ctx context.Context, argv []string, stdout, stderr io.Writer) error) {
				f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
					return fn(ctx, argv, stdout, stderr)
				}
			},
			write: func(p, content string) { f.put(id, p, content) },
			read:  func(p string) (string, bool) { return f.file(id, p) },
			left:  func() []string { l.p.shutdown(); return f.leftovers(l.p.token) },
		}
	}},
	{"docker", func(t *testing.T, spec Spec) (Session, *conformFake) {
		f := newFakeDaemon(t, "docker")
		spec.Workdir = workdir(t)
		sess, err := containerOn(t, f).Start(context.Background(), spec)
		require.NoError(t, err)
		id := sess.(*containerSession).id
		return sess, &conformFake{id: id,
			agent: func(fn func(ctx context.Context, argv []string, stdout, stderr io.Writer) error) {
				f.agent = func(ctx context.Context, ctr string, argv []string, stdout, stderr io.Writer) error {
					return fn(ctx, argv, stdout, stderr)
				}
			},
			write: func(p, content string) { f.write(id, p, content) },
			read:  func(p string) (string, bool) { return f.read(id, p) },
			left:  f.leftovers,
		}
	}},
}

func TestSessionConformance(t *testing.T) {
	for _, d := range conformDrivers {
		t.Run(d.name+"/push a directory to a new path: its contents land there", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			src := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(src, "bin"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(src, "bin", "tool"), []byte("tool"), 0o755))
			require.NoError(t, sess.Push(context.Background(), src, "/pats-prepare"))
			got, ok := f.read("/pats-prepare/bin/tool")
			assert.That(t, ok && got == "tool", "got %q", got)
		})
		t.Run(d.name+"/push a directory to an existing path: its contents land there", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			f.write("/opt/keep", "kept")
			src := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(src, "new"), []byte("new"), 0o644))
			require.NoError(t, sess.Push(context.Background(), src, "/opt"))
			got, ok := f.read("/opt/new")
			assert.That(t, ok && got == "new", "got %q", got)
			got, ok = f.read("/opt/keep")
			assert.That(t, ok && got == "kept", "an existing file went: %q", got)
		})
		t.Run(d.name+"/push a file: it lands at the path", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			src := filepath.Join(t.TempDir(), "bundle.pem")
			require.NoError(t, os.WriteFile(src, []byte("pem"), 0o644))
			require.NoError(t, sess.Push(context.Background(), src, "/sbx-ca/ca.pem"))
			got, ok := f.read("/sbx-ca/ca.pem")
			assert.That(t, ok && got == "pem", "got %q", got)
		})
		t.Run(d.name+"/pull a directory: its contents land in hostDir", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			f.write("/out/sub/result", "42")
			dst := t.TempDir()
			require.NoError(t, sess.Pull(context.Background(), "/out", dst))
			b, err := os.ReadFile(filepath.Join(dst, "sub", "result"))
			require.NoError(t, err, "what landed: %v", tree(dst))
			assert.Equal(t, string(b), "42")
		})
		t.Run(d.name+"/a non-zero exit is the command's, not an error", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			f.agent(func(ctx context.Context, argv []string, stdout, stderr io.Writer) error {
				io.WriteString(stdout, "out")
				io.WriteString(stderr, "err")
				return exitStatus(3)
			})
			var out, errb strings.Builder
			code, err := sess.Exec(context.Background(), Cmd{Argv: []string{"false"}}, &out, &errb)
			require.NoError(t, err)
			assert.Equal(t, code, 3)
			assert.Equal(t, out.String(), "out")
			assert.Equal(t, errb.String(), "err")
		})
		t.Run(d.name+"/close twice, nothing left", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: lxdEgressModes[2].egress})
			require.NoError(t, sess.Close())
			require.NoError(t, sess.Close())
			assert.Equal(t, strings.Join(f.left(), "\n"), "")
		})
	}
}

// tree lists the files under dir, for a failure message.
func tree(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}
