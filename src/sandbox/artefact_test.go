package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// served counts the fetches of one body, behind a redirect.
func served(t *testing.T, body []byte) (url string, hits *int) {
	t.Helper()
	hits = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/file", http.StatusFound)
			return
		}
		*hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/redirect", hits
}

// a fetched artefact is checked against its sha and kept by it; a cached one
// is hashed again before use, and fetched once more when it changed.
func TestFetchArtefact(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	body := []byte("the bytes of a cli")
	url, hits := served(t, body)
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", URL: url, SHA256: sha(body)}

	file, err := FetchArtefact(context.Background(), a, nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(file), a.SHA256)
	got, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(got), string(body))
	assert.Equal(t, *hits, 1) // the redirect was followed

	// a full cache needs no source.
	gone := a
	gone.URL = "http://127.0.0.1:1/nothing-listens-here"
	again, err := FetchArtefact(context.Background(), gone, nil)
	require.NoError(t, err)
	assert.Equal(t, again, file)
	assert.Equal(t, *hits, 1)

	// a cached file that changed is not used: it is fetched once more.
	require.NoError(t, os.WriteFile(file, []byte("tampered"), 0o644))
	_, err = FetchArtefact(context.Background(), a, nil)
	require.NoError(t, err)
	assert.Equal(t, *hits, 2)
	got, err = os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(got), string(body))
}

// bytes that do not hash to the pin are an error naming the cli, the version,
// the arch, the url and both shas, and nothing of them is kept.
func TestFetchArtefactMismatch(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	body := []byte("not what was pinned")
	url, _ := served(t, body)
	pinned := sha([]byte("what was pinned"))
	_, err := FetchArtefact(context.Background(), Artefact{CLI: "tool", Version: "1.2.3", Arch: "arm64", URL: url, SHA256: pinned}, nil)
	for _, want := range []string{"tool 1.2.3 for arm64", url, "sha256 is " + sha(body), "the pin says " + pinned} {
		assert.Error(t, err, want)
	}
	left, err := os.ReadDir(filepath.Join(cache, "pats", "harness"))
	require.NoError(t, err)
	assert.Len(t, left, 0)

	// a source that answers with an error, and one that sends too much.
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err = FetchArtefact(context.Background(), Artefact{CLI: "tool", Version: "9", Arch: "amd64", URL: srv.URL, SHA256: pinned}, nil)
	assert.Error(t, err, "the source answered 404")

	old := maxArtefact
	maxArtefact = 8
	defer func() { maxArtefact = old }()
	_, err = FetchArtefact(context.Background(), Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", URL: url, SHA256: sha(body)}, nil)
	assert.Error(t, err, "more than 8 bytes, refused")
}

type tarEntry struct {
	name, link, body string
	mode             int64
	typ              byte
}

// tarGz writes a gzipped tar of entries to a file and returns its path.
func tarGz(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body))}
		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	p := filepath.Join(t.TempDir(), "artefact.tgz")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o644))
	return p
}

// a tree is the unpacked artefact under opt/pats/harness/<cli> and a link to
// its executable in usr/local/bin, as the image will see it.
func TestBuildTree(t *testing.T) {
	file := tarGz(t,
		tarEntry{name: "package/", mode: 0o755, typ: tar.TypeDir},
		tarEntry{name: "package/bin/tool", mode: 0o4755, typ: tar.TypeReg, body: "#!/bin/sh\n"},
		tarEntry{name: "package/README.md", mode: 0o644, typ: tar.TypeReg, body: "read me"},
	)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "opt"), 0o700)) // as a tight umask would make it
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", SHA256: "aa", Bin: "package/bin/tool"}
	require.NoError(t, BuildTree(a, file, dir))

	// the directories above the cli's own are an image's (/, /opt, /usr):
	// a copy may give them these modes, so they are never this machine's.
	for _, rel := range []string{".", "opt", "opt/pats", "opt/pats/harness", "usr", "usr/local", "usr/local/bin"} {
		fi, err := os.Stat(filepath.Join(dir, rel))
		require.NoError(t, err)
		assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o755), rel)
	}

	exe := filepath.Join(dir, "opt", "pats", "harness", "tool", "package", "bin", "tool")
	fi, err := os.Stat(exe)
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o755)) // the setuid bit is dropped
	assert.Equal(t, fi.Mode()&os.ModeSetuid, os.FileMode(0))
	target, err := os.Readlink(filepath.Join(dir, "usr", "local", "bin", "tool"))
	require.NoError(t, err)
	assert.Equal(t, target, "/opt/pats/harness/tool/package/bin/tool")

	// the bin an entry names has to be there, and be an executable.
	missing := a
	missing.Bin = "package/bin/nope"
	assert.Error(t, BuildTree(missing, file, t.TempDir()), "bin package/bin/nope is not in the artefact")
	plain := a
	plain.Bin = "package/README.md"
	assert.Error(t, BuildTree(plain, file, t.TempDir()), "is not an executable file")

	// a bin that is a link out of the tree is not the artefact's executable,
	// though this machine has a file where it points.
	linked := tarGz(t, tarEntry{name: "package/tool", typ: tar.TypeSymlink, link: "/bin/sh"})
	out := a
	out.Bin = "package/tool"
	assert.Error(t, BuildTree(out, linked, t.TempDir()), "bin package/tool is not in the artefact")

	// a tree is built afresh: what an earlier artefact left is gone, and
	// the modes are a reader's whatever was unpacked.
	next := tarGz(t,
		tarEntry{name: "package/bin/tool", mode: 0o700, typ: tar.TypeReg, body: "#!/bin/sh\n"},
		tarEntry{name: "package/data", mode: 0o600, typ: tar.TypeReg, body: "d"},
	)
	require.NoError(t, BuildTree(a, next, dir))
	_, err = os.Stat(filepath.Join(dir, "opt", "pats", "harness", "tool", "package", "README.md"))
	assert.That(t, os.IsNotExist(err), "the earlier artefact's file is still in the tree")
	for name, want := range map[string]os.FileMode{"package/bin/tool": 0o755, "package/data": 0o644, "package/bin": 0o755, "package": 0o755} {
		fi, err := os.Stat(filepath.Join(dir, "opt", "pats", "harness", "tool", filepath.FromSlash(name)))
		require.NoError(t, err)
		assert.Equal(t, fi.Mode().Perm(), want, name)
	}
	assert.Error(t, BuildTree(Artefact{CLI: "../x", Version: "1"}, next, t.TempDir()), "is not a cli's name")

	// an artefact with no bin is the executable itself.
	raw := filepath.Join(t.TempDir(), "raw")
	require.NoError(t, os.WriteFile(raw, []byte("\x7fELF"), 0o600))
	dir = t.TempDir()
	require.NoError(t, BuildTree(Artefact{CLI: "omp", Version: "1"}, raw, dir))
	fi, err = os.Stat(filepath.Join(dir, "opt", "pats", "harness", "omp", "omp"))
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o755))
	target, err = os.Readlink(filepath.Join(dir, "usr", "local", "bin", "omp"))
	require.NoError(t, err)
	assert.Equal(t, target, "/opt/pats/harness/omp/omp")
}

// an artefact is not pats's own: an entry that would leave the tree is
// refused, by its name or by its link.
func TestBuildTreeRefusesEscapes(t *testing.T) {
	for name, e := range map[string]tarEntry{
		"a path that climbs": {name: "../outside", mode: 0o644, typ: tar.TypeReg, body: "x"},
		"a link that climbs": {name: "package/link", typ: tar.TypeSymlink, link: "../../../../etc/passwd"},
	} {
		file := tarGz(t, tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "x"}, e)
		dir := t.TempDir()
		err := BuildTree(Artefact{CLI: "tool", Version: "1", Bin: "package/tool"}, file, dir)
		assert.That(t, err != nil, "%s: unpacked", name)
		_, serr := os.Stat(filepath.Join(dir, "opt", "pats", "outside"))
		assert.That(t, os.IsNotExist(serr), "%s: wrote outside the cli's dir", name)
	}
}

// a tree's id moves with what decides its contents, bin included, and not
// with where the artefact is from.
func TestTreeID(t *testing.T) {
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", URL: "https://a.example/x", SHA256: "aa", Bin: "package/tool"}
	moved := a
	moved.URL = "https://mirror.example/x"
	assert.Equal(t, moved.TreeID(), a.TreeID())
	for name, change := range map[string]func(*Artefact){
		"bin":     func(x *Artefact) { x.Bin = "package/bin/tool" },
		"sha":     func(x *Artefact) { x.SHA256 = "bb" },
		"version": func(x *Artefact) { x.Version = "1.2.4" },
		"cli":     func(x *Artefact) { x.CLI = "other" },
		"arch":    func(x *Artefact) { x.Arch = "arm64" },
	} {
		b := a
		change(&b)
		assert.That(t, b.TreeID() != a.TreeID(), "the id did not move with the %s", name)
	}
}
