//go:build !nobwrap

package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lczyk/assert/require"
)

// bwrap's rows of the cross-driver tables, and its tests that live beside
// shared ones, so a build without bwrap drops them with the driver.

func init() {
	driverCases = append(driverCases,
		// bwrap has no daemon to fake: its store is a cache dir of the test's,
		// and its registry a local server.
		driverCase{name: "bwrap", noBuild: true,
			open: func(t *testing.T) (Driver, func() int) {
				t.Setenv("XDG_CACHE_HOME", t.TempDir())
				p := testProcess(t)
				p.reg, _ = imageRegistry(t, nil, tarLayer(t, entry{name: "etc/os-release", body: "ubuntu"}))
				d, err := newDriver(p, nil, Options{Driver: BwrapDriver})
				require.NoError(t, err)
				return d, nil
			},
			// a session holds its image's lock, shared, for as long as it lives.
			inUse: func(t *testing.T, d Driver, ref string) {
				img, err := d.Resolve(context.Background(), ref)
				require.NoError(t, err)
				st, err := newBwrapStore()
				require.NoError(t, err)
				unlock, err := st.lock(context.Background(), img.ID, false, false)
				require.NoError(t, err)
				t.Cleanup(unlock)
			},
		},
	)
	conformDrivers = append(conformDrivers, conformDriver{"bwrap", bwrapConform})
	execBackends = append(execBackends,
		execBackend{
			name: "bwrap",
			session: func(t *testing.T, p *process, r runner) Session {
				return &bwrapSession{d: &bwrapDriver{p: p, r: r}, rootfs: "/store/rootfs/sha256-x", dir: t.TempDir(), workdir: "/work"}
			},
			kills: true,
		},
	)
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

// BenchmarkUnpackLayer is a bwrap pull's work per layer once it is on disk:
// gunzip and untar into the rootfs. "one-pass" is the same layer read once
// and thrown away, the floor the two passes of unpackLayer sit above.
func BenchmarkUnpackLayer(b *testing.B) {
	gz, raw := benchLayer(b, 4000, 8<<10)
	open := func() (io.ReadCloser, error) {
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return nil, err
		}
		return zr, nil
	}
	b.Run("unpack", func(b *testing.B) {
		b.SetBytes(raw)
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			root, err := os.MkdirTemp(b.TempDir(), "rootfs")
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if err := unpackLayer(open, root); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = os.RemoveAll(root)
			b.StartTimer()
		}
	})
	b.Run("one-pass-read", func(b *testing.B) {
		b.SetBytes(raw)
		for b.Loop() {
			r, err := open()
			if err != nil {
				b.Fatal(err)
			}
			tr := tar.NewReader(r)
			for {
				if _, err := tr.Next(); err != nil {
					break
				}
				_, _ = io.Copy(io.Discard, tr)
			}
			r.Close()
		}
	})
}

// BenchmarkTar is lxd's Push and Pull of a workdir, the daemon aside: the
// tree written as a tar, and a tar unpacked onto the host.

// FuzzUnpackLayer: bwrap's store unpacks a registry's layers on the host.
func FuzzUnpackLayer(f *testing.F) {
	fuzzTarSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzUnpack(t, data, func(archive []byte, dest string) error {
			return unpackLayer(opener(archive), dest)
		})
	})
}
