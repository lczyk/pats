package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// benchTree writes n files of size bytes each under root, spread over dirs
// directories two levels down, and returns the bytes written.
func benchTree(b *testing.B, root string, dirs, n, size int) int64 {
	b.Helper()
	body := bytes.Repeat([]byte("pats\n"), size/5+1)[:size]
	for i := range n {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%dirs), fmt.Sprintf("sub%d", i%7))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.go", i)), body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return int64(n * size)
}

// BenchmarkContextHash is what `pats run` and `pats init` pay per build
// context before anything starts: every file not ignored, read and hashed.
// with no ignore file the vendored tree is hashed too; ignored, it is pruned,
// until one negation in the file has it walked entry by entry again.
func BenchmarkContextHash(b *testing.B) {
	ctx := b.TempDir()
	kept := benchTree(b, filepath.Join(ctx, "src"), 20, 2000, 4<<10)
	benchTree(b, filepath.Join(ctx, "node_modules"), 50, 10000, 512)
	dockerfile := filepath.Join(ctx, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM ubuntu:26.04\nCOPY . /src\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	ignores := map[string]string{
		"no-ignore-file":   "",
		"ignored-pruned":   "node_modules\n**/*.log\n.git\n",
		"ignored-negation": "node_modules\n**/*.log\n.git\n!src/keep.log\n",
	}
	for _, name := range []string{"no-ignore-file", "ignored-pruned", "ignored-negation"} {
		b.Run(name, func(b *testing.B) {
			ignore := filepath.Join(ctx, ".dockerignore")
			_ = os.Remove(ignore)
			if body := ignores[name]; body != "" {
				if err := os.WriteFile(ignore, []byte(body), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			b.SetBytes(kept)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ContextHash(ctx, dockerfile); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchLayer is a gzipped layer of n files of size bytes each, as a registry
// serves one.
func benchLayer(b *testing.B, n, size int) (gz []byte, raw int64) {
	b.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	// hex of random bytes: about half its size gzipped, as binaries are.
	rng := rand.New(rand.NewPCG(1, 2))
	body := make([]byte, size)
	for i := range n {
		for j := range body {
			body[j] = "0123456789abcdef"[rng.IntN(16)]
		}
		name := fmt.Sprintf("usr/lib/pkg%02d/file%04d.so", i%40, i)
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(size)}); err != nil {
			b.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			b.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes(), int64(n * size)
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
func BenchmarkTar(b *testing.B) {
	src := b.TempDir()
	size := benchTree(b, src, 20, 2000, 4<<10)
	fi, err := os.Stat(src)
	if err != nil {
		b.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeTar(&archive, src, fi); err != nil {
		b.Fatal(err)
	}
	b.Run("write", func(b *testing.B) {
		b.SetBytes(size)
		b.ReportAllocs()
		for b.Loop() {
			if err := writeTar(io.Discard, src, fi); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read", func(b *testing.B) {
		b.SetBytes(size)
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			dest, err := os.MkdirTemp(b.TempDir(), "pull")
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if err := readTar(bytes.NewReader(archive.Bytes()), dest); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = os.RemoveAll(dest)
			b.StartTimer()
		}
	})
}
