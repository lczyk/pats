package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	id string // the sandbox, as the fake names it
	// exits readies a command that writes stdout and stderr and exits code,
	// and returns the argv that runs it.
	exits func(code int, stdout, stderr string) []string
	write func(p, content string)
	read  func(p string) (string, bool)
	left  func() []string
	// at is where a path of the sandbox is to Push and Pull: itself, but a
	// host dir under host, whose sandbox is the host.
	at func(p string) string
}

func samePath(p string) string { return p }

// printsAndExits is an agent a fake runs: it writes both streams and exits code.
func printsAndExits(code int, out, errOut string) func(stdout, stderr io.Writer) error {
	return func(stdout, stderr io.Writer) error {
		io.WriteString(stdout, out)
		io.WriteString(stderr, errOut)
		return exitStatus(code)
	}
}

var conformDrivers = []conformDriver{
	{"lxd", func(t *testing.T, spec Spec) (Session, *conformFake) {
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{ProxyBin: proxyBin(t)})
		sess, err := l.Start(context.Background(), spec)
		require.NoError(t, err)
		id := sess.(*lxdSession).name
		return sess, &conformFake{id: id, at: samePath,
			exits: func(code int, out, errOut string) []string {
				f.agent = func(_ context.Context, _ string, _ []string, stdout, stderr io.Writer) error {
					return printsAndExits(code, out, errOut)(stdout, stderr)
				}
				return []string{"false"}
			},
			write: func(p, content string) { f.put(id, p, content) },
			read:  func(p string) (string, bool) { return f.file(id, p) },
			left:  func() []string { l.p.shutdown(); return f.leftovers(l.p.token) },
		}
	}},
	containerConform("docker"),
	containerConform("podman"),
	{"bwrap", bwrapConform},
	{"host", hostConform},
}

// bwrapConform is a bwrap session on a fake bwrap that keeps a tree for the
// sandbox's root: a push's cp lands in it, and a path under a bind is the
// bind's host side, as in the real one. bwrap pulls bound paths only
// (docs/spec/bwrap.md), so /out is a bind here.
func bwrapConform(t *testing.T, spec Spec) (Session, *conformFake) {
	root, out, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "session")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	binds := []bind{{host: out, path: "/out"}}
	host := func(p string) string {
		for _, b := range append([]bind{{host: spec.Workdir, path: WorkMount}}, binds...) {
			if rel, err := filepath.Rel(b.path, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return filepath.Join(b.host, rel)
			}
		}
		return filepath.Join(root, p)
	}
	var agent func(stdout, stderr io.Writer) error
	r := runFunc(func(_ context.Context, _ io.Reader, stdout, stderr io.Writer, args ...string) error {
		argv := args[slices.Index(args, "pats-env")+1:]
		if len(argv) != 6 || argv[3] != "push" {
			if agent != nil {
				return agent(stdout, stderr)
			}
			return nil
		}
		// Push: the host side is bound at the staging path, and cp -a
		// copies it, a directory's contents or the one file.
		i := slices.Index(args, argv[4])
		if i < 2 || args[i-2] != "--ro-bind" {
			t.Errorf("fake bwrap: a push with no source bound at %s: %v", argv[4], args)
			return exitStatus(1)
		}
		// the script itself, on the host sides of its two paths.
		out, err := exec.Command(argv[0], argv[1], argv[2], argv[3], args[i-1], host(argv[5])).CombinedOutput()
		if err != nil {
			t.Logf("fake bwrap: push: %v: %s", err, out)
			return exitStatus(1)
		}
		return nil
	})
	unlocked := false
	sess := &bwrapSession{d: &bwrapDriver{p: testProcess(t), r: r}, rootfs: t.TempDir(), dir: dir, workdir: spec.Workdir,
		binds: binds, net: netNone, unlock: func() { unlocked = true }}
	return sess, &conformFake{id: filepath.Base(dir), at: samePath,
		exits: func(code int, out, errOut string) []string {
			agent = printsAndExits(code, out, errOut)
			return []string{"false"}
		},
		write: func(p, content string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(host(p)), 0o755))
			require.NoError(t, os.WriteFile(host(p), []byte(content), 0o644))
		},
		read: func(p string) (string, bool) {
			b, err := os.ReadFile(host(p))
			return string(b), err == nil
		},
		left: func() []string {
			var left []string
			if _, err := os.Stat(dir); err == nil {
				left = append(left, "the session dir "+dir)
			}
			if !unlocked {
				left = append(left, "the image's lock")
			}
			return left
		},
	}
}

// hostConform is a host session, which has no fake: its commands are real
// processes and its sandbox is the host, so the paths of the table are dirs
// under a root of the test's. the proxy child is TestHostEgress's.
func hostConform(t *testing.T, spec Spec) (Session, *conformFake) {
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	spec.Egress = Egress{None: true}
	sess, err := hostOn(t, Options{}).Start(context.Background(), spec)
	require.NoError(t, err)
	return sess, &conformFake{id: "host", at: at,
		exits: func(code int, out, errOut string) []string {
			return []string{"/bin/sh", "-c", fmt.Sprintf("printf %%s %s; printf %%s %s >&2; exit %d", shQuote(out), shQuote(errOut), code)}
		},
		write: func(p, content string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(at(p)), 0o755))
			require.NoError(t, os.WriteFile(at(p), []byte(content), 0o644))
		},
		read: func(p string) (string, bool) {
			b, err := os.ReadFile(at(p))
			return string(b), err == nil
		},
		// a host session makes nothing to leave but what it runs.
		left: func() []string {
			if _, err := sess.Exec(context.Background(), Cmd{Argv: []string{"true"}}, io.Discard, io.Discard); !errors.Is(err, ErrLaunch) {
				return []string{"a closed session that still runs commands"}
			}
			return nil
		},
	}
}

func containerConform(driver string) conformDriver {
	return conformDriver{driver, func(t *testing.T, spec Spec) (Session, *conformFake) {
		f := newFakeDaemon(t, driver)
		spec.Workdir = workdir(t)
		sess, err := containerOn(t, f).Start(context.Background(), spec)
		require.NoError(t, err)
		id := sess.(*containerSession).id
		return sess, &conformFake{id: id, at: samePath,
			exits: func(code int, out, errOut string) []string {
				f.agent = func(_ context.Context, _ string, _ []string, stdout, stderr io.Writer) error {
					return printsAndExits(code, out, errOut)(stdout, stderr)
				}
				return []string{"false"}
			},
			write: func(p, content string) { f.write(id, p, content) },
			read:  func(p string) (string, bool) { return f.read(id, p) },
			left:  f.leftovers,
		}
	}}
}

func TestSessionConformance(t *testing.T) {
	for _, d := range conformDrivers {
		t.Run(d.name+"/push a directory to a new path: its contents land there", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			src := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(src, "bin"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(src, "bin", "tool"), []byte("tool"), 0o755))
			require.NoError(t, sess.Push(context.Background(), src, f.at("/pats-prepare")))
			got, ok := f.read("/pats-prepare/bin/tool")
			assert.That(t, ok && got == "tool", "got %q", got)
		})
		t.Run(d.name+"/push a directory to an existing path: its contents land there", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			f.write("/opt/keep", "kept")
			src := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(src, "new"), []byte("new"), 0o644))
			require.NoError(t, sess.Push(context.Background(), src, f.at("/opt")))
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
			require.NoError(t, sess.Push(context.Background(), src, f.at("/sbx-ca/ca.pem")))
			got, ok := f.read("/sbx-ca/ca.pem")
			assert.That(t, ok && got == "pem", "got %q", got)
		})
		t.Run(d.name+"/pull a directory: its contents land in hostDir", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			f.write("/out/sub/result", "42")
			dst := t.TempDir()
			require.NoError(t, sess.Pull(context.Background(), f.at("/out"), dst))
			b, err := os.ReadFile(filepath.Join(dst, "sub", "result"))
			require.NoError(t, err, "what landed: %v", tree(dst))
			assert.Equal(t, string(b), "42")
		})
		t.Run(d.name+"/a non-zero exit is the command's, not an error", func(t *testing.T) {
			sess, f := d.start(t, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			defer sess.Close()
			argv := f.exits(3, "out", "err")
			var out, errb strings.Builder
			code, err := sess.Exec(context.Background(), Cmd{Argv: argv}, &out, &errb)
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
