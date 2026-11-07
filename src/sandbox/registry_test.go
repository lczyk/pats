package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// fakeRegistry serves one repo the way ghcr does for an anonymous pull: a
// token, the tag's digest and type on HEAD, manifests and blobs by digest.
type fakeRegistry struct {
	tokenStatus int
	tags        map[string]string // tag -> digest
	types       map[string]string // digest -> media type
	blobs       map[string][]byte // digest -> content, manifests and layers
	tamper      map[string]bool   // digests served with altered bytes
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{tokenStatus: http.StatusOK, tags: map[string]string{}, types: map[string]string{},
		blobs: map[string][]byte{}, tamper: map[string]bool{}}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *fakeRegistry) put(b []byte, mediaType string) string {
	d := digestOf(b)
	r.blobs[d], r.types[d] = b, mediaType
	return d
}

// layer is a gzipped tar of files.
func layer(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// image puts a manifest whose top layer holds files, and returns its digest.
func (r *fakeRegistry) image(t *testing.T, files map[string]string) string {
	l := r.put(layer(t, files), "application/vnd.oci.image.layer.v1.tar+gzip")
	base := r.put(layer(t, map[string]string{"etc/base": "x"}), "application/vnd.oci.image.layer.v1.tar+gzip")
	m, _ := json.Marshal(map[string]any{"layers": []map[string]string{{"digest": base}, {"digest": l}}})
	return r.put(m, "application/vnd.oci.image.manifest.v1+json")
}

// index puts an index over per-arch manifests and tags it.
func (r *fakeRegistry) index(tag string, arches map[string]string) {
	var ms []map[string]any
	for arch, d := range arches {
		ms = append(ms, map[string]any{"digest": d, "platform": map[string]string{"os": "linux", "architecture": arch}})
	}
	b, _ := json.Marshal(map[string]any{"manifests": ms})
	r.tags[tag] = r.put(b, "application/vnd.oci.image.index.v1+json")
}

func (r *fakeRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		if r.tokenStatus != http.StatusOK {
			w.WriteHeader(r.tokenStatus)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		return
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/org/pats/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	if kind == "manifests" && req.Method == http.MethodHead {
		d, ok := r.tags[ref]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Type", r.types[d])
		return
	}
	b, ok := r.blobs[ref]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.tamper[ref] {
		b = append(append([]byte(nil), b...), ' ')
	}
	w.Write(b)
}

func (r *fakeRegistry) serve(t *testing.T) registry {
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return registry{client: srv.Client(), url: func(string) string { return srv.URL }}
}

// the binary for the server's arch comes out of the top layer of the image
// the index names for it; every manifest and the layer are checked against
// their digests first, and nothing reaches dst unless all of it holds.
func TestFetchImageFile(t *testing.T) {
	const ref = "ghcr.io/org/pats:v1"
	setup := func(t *testing.T) *fakeRegistry {
		r := newFakeRegistry()
		r.index("v1", map[string]string{
			"amd64": r.image(t, map[string]string{"pats": "amd64 binary"}),
			"arm64": r.image(t, map[string]string{"./pats": "arm64 binary"}),
		})
		return r
	}
	fetch := func(t *testing.T, r *fakeRegistry, arch string) (string, error) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "pats")
		err := r.serve(t).fetchImageFile(context.Background(), ref, arch, "pats", dst)
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			assert.Equal(t, e.Name(), "pats", "a temp file was left")
		}
		b, rerr := os.ReadFile(dst)
		if err != nil {
			assert.That(t, os.IsNotExist(rerr), "dst written on a failed fetch")
			return "", err
		}
		st, _ := os.Stat(dst)
		assert.Equal(t, st.Mode().Perm(), os.FileMode(0o755))
		return string(b), nil
	}

	t.Run("each arch", func(t *testing.T) {
		r := setup(t)
		got, err := fetch(t, r, "amd64")
		require.NoError(t, err)
		assert.Equal(t, got, "amd64 binary")
		got, err = fetch(t, r, "arm64")
		require.NoError(t, err)
		assert.Equal(t, got, "arm64 binary")
	})
	t.Run("a single manifest, no index", func(t *testing.T) {
		r := newFakeRegistry()
		r.tags["v1"] = r.image(t, map[string]string{"pats": "only"})
		got, err := fetch(t, r, "amd64")
		require.NoError(t, err)
		assert.Equal(t, got, "only")
	})
	t.Run("no image for the arch", func(t *testing.T) {
		_, err := fetch(t, setup(t), "riscv64")
		assert.Error(t, err, "has no linux/riscv64 image")
	})
	t.Run("only another os for the arch", func(t *testing.T) {
		r := newFakeRegistry()
		d := r.image(t, map[string]string{"pats": "windows binary"})
		b, _ := json.Marshal(map[string]any{"manifests": []map[string]any{
			{"digest": d, "platform": map[string]string{"os": "windows", "architecture": "amd64"}}}})
		r.tags["v1"] = r.put(b, "application/vnd.oci.image.index.v1+json")
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "has no linux/amd64 image")
	})
	t.Run("the index tampered with", func(t *testing.T) {
		r := setup(t)
		r.tamper[r.tags["v1"]] = true
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "digest")
	})
	t.Run("a manifest tampered with", func(t *testing.T) {
		r := setup(t)
		for d, typ := range r.types {
			if strings.Contains(typ, "manifest.v1") {
				r.tamper[d] = true
			}
		}
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "digest")
	})
	t.Run("a layer tampered with", func(t *testing.T) {
		r := setup(t)
		for d, typ := range r.types {
			if strings.Contains(typ, "layer") {
				r.tamper[d] = true
			}
		}
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "layer digest")
	})
	t.Run("no binary in the top layer", func(t *testing.T) {
		r := newFakeRegistry()
		r.tags["v1"] = r.image(t, map[string]string{"other": "x"})
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "no pats in its top layer")
	})
	t.Run("the token refused", func(t *testing.T) {
		r := setup(t)
		r.tokenStatus = http.StatusForbidden
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "registry token: 403")
	})
	t.Run("no such tag", func(t *testing.T) {
		r := setup(t)
		delete(r.tags, "v1")
		_, err := fetch(t, r, "amd64")
		assert.Error(t, err, "404")
	})
}
