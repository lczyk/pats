package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// entry is one tar entry of a crafted layer.
type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarLayer(t *testing.T, es ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		require.NoError(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return b.Bytes()
}

// opener reopens a crafted layer for each of unpack's passes.
func opener(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func TestUnpackLayers(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, unpackLayer(opener(tarLayer(t,
		entry{name: "etc/", typ: tar.TypeDir},
		entry{name: "etc/keep", body: "keep"},
		entry{name: "etc/gone", body: "gone"},
		entry{name: "opt/", typ: tar.TypeDir},
		entry{name: "opt/old", body: "old"},
		entry{name: "usr/bin/su", body: "su", mode: 0o4755},
		entry{name: "dev/null", typ: tar.TypeChar},
		entry{name: "tmp/", typ: tar.TypeDir, mode: 0o1777},
		entry{name: "bin", typ: tar.TypeSymlink, link: "usr/bin"},
		entry{name: "etc/abs", typ: tar.TypeSymlink, link: "/etc/keep"},
	)), root))
	require.NoError(t, unpackLayer(opener(tarLayer(t,
		entry{name: "etc/.wh.gone"},
		entry{name: "opt/.wh..wh..opq"},
		entry{name: "opt/new", body: "new"},
		entry{name: "etc/hard", typ: tar.TypeLink, link: "etc/keep"},
		// the layer's own file before its opaque marker: the marker clears
		// lower layers only.
		entry{name: "srv/mine", body: "mine"},
		entry{name: "srv/.wh..wh..opq"},
	)), root))
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	assert.Equal(t, read("etc/keep"), "keep")
	assert.That(t, strings.HasPrefix(read("etc/gone"), "<"), "whiteout left etc/gone")
	assert.That(t, strings.HasPrefix(read("opt/old"), "<"), "opaque dir kept opt/old")
	assert.Equal(t, read("opt/new"), "new")
	assert.Equal(t, read("etc/hard"), "keep")
	assert.Equal(t, read("srv/mine"), "mine")
	fi, err := os.Stat(filepath.Join(root, "usr/bin/su"))
	require.NoError(t, err)
	assert.Equal(t, fi.Mode()&(os.ModeSetuid|os.ModeSetgid), os.FileMode(0))
	_, err = os.Lstat(filepath.Join(root, "dev/null"))
	assert.That(t, errors.Is(err, os.ErrNotExist), "a device node was made")
	fi, err = os.Stat(filepath.Join(root, "tmp"))
	require.NoError(t, err)
	assert.Equal(t, fi.Mode()&os.ModeSticky, os.ModeSticky)
	link, err := os.Readlink(filepath.Join(root, "etc/abs"))
	require.NoError(t, err)
	assert.Equal(t, link, "/etc/keep") // absolute: resolved inside the rootfs at run time
}

func TestUnpackRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]entry{
		"dotdot":         {{name: "../evil", body: "x"}},
		"nested dotdot":  {{name: "a/../../evil", body: "x"}},
		"relative link":  {{name: "a/up", typ: tar.TypeSymlink, link: "../../evil"}},
		"through a link": {{name: "out", typ: tar.TypeSymlink, link: outside}, {name: "out/evil", body: "x"}},
		"hard link out":  {{name: "h", typ: tar.TypeLink, link: "../etc/passwd"}},
		"whiteout up":    {{name: ".wh.."}},
		"whiteout dot":   {{name: "a/.wh.."}},
	}
	for name, es := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			err := unpackLayer(opener(tarLayer(t, es...)), root)
			assert.ErrorIs(t, err, errEscape)
			_, serr := os.Stat(filepath.Join(outside, "evil"))
			assert.That(t, errors.Is(serr, os.ErrNotExist), "a file landed outside the rootfs")
		})
	}
}

// imageRegistry serves one single-arch image of the given layers.
func imageRegistry(t *testing.T, env []string, layers ...[]byte) (registry, string) {
	t.Helper()
	blobs := map[string][]byte{}
	digest := func(b []byte) string {
		s := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(s[:])
	}
	var descs []layerDesc
	for _, l := range layers {
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		_, _ = w.Write(l)
		_ = w.Close()
		d := digest(gz.Bytes())
		blobs[d] = gz.Bytes()
		descs = append(descs, layerDesc{Digest: d, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"})
	}
	cfg, _ := json.Marshal(map[string]any{"config": map[string]any{"Env": env}})
	blobs[digest(cfg)] = cfg
	man, _ := json.Marshal(map[string]any{"config": layerDesc{Digest: digest(cfg)}, "layers": descs})
	md := digest(man)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", md)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if r.Method != http.MethodHead {
				_, _ = w.Write(man)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			b, ok := blobs[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return registry{client: srv.Client(), url: func(string) string { return srv.URL }}, host + "/pats/img:latest"
}

func TestBwrapStorePull(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	reg, ref := imageRegistry(t, []string{"PATH=/opt/tools/bin:/usr/bin"},
		tarLayer(t, entry{name: "etc/os-release", body: "ubuntu"}))
	st, err := newBwrapStore()
	require.NoError(t, err)
	ri, err := reg.image(t.Context(), ref, "amd64")
	require.NoError(t, err)
	fetched, err := st.pull(ri, false, nil)
	require.NoError(t, err)
	assert.Equal(t, fetched, true)
	b, err := os.ReadFile(filepath.Join(st.rootfs(ri.manifest), "etc/os-release"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "ubuntu")
	env, err := st.imageEnv(ri.manifest)
	require.NoError(t, err)
	assert.EqualArrays(t, env, []string{"PATH=/opt/tools/bin:/usr/bin"})
	id, _, ok := st.resolveRef(ref)
	assert.That(t, ok, "ref not recorded")
	assert.Equal(t, id, ri.manifest)

	// a second pull of the same digest converges on the first.
	fetched, err = st.pull(ri, false, nil)
	require.NoError(t, err)
	assert.Equal(t, fetched, false)

	// a session's shared lock keeps the image; a crashed holder's lock
	// goes with it.
	unlock, err := st.lock(ri.manifest, false, false)
	require.NoError(t, err)
	assert.Error(t, st.remove(ri.manifest), "in use")
	unlock()
	require.NoError(t, st.remove(ri.manifest))
	assert.Equal(t, st.has(ri.manifest), false)
}

// a crash mid-unpack leaves only <id>.new, which a sweep removes once its
// lock is free, and never while an unpack holds it.
func TestBwrapStoreSweepNew(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st, err := newBwrapStore()
	require.NoError(t, err)
	id := "sha256:" + strings.Repeat("a", 64)
	half := st.rootfs(id) + ".new"
	require.NoError(t, os.MkdirAll(filepath.Join(half, "etc"), 0o755))
	require.NoError(t, os.MkdirAll(st.rootfs(id)+".dl", 0o700))
	require.NoError(t, os.WriteFile(st.rootfs(id)+".dl/.pats-layer-1", []byte("half a layer"), 0o600))
	require.NoError(t, os.Chmod(filepath.Join(half, "etc"), 0o500))
	assert.Equal(t, st.has(id), false)
	unlock, err := st.lock(id, true, false)
	require.NoError(t, err)
	assert.Equal(t, len(st.sweepNew(false)), 0)
	unlock()
	assert.Equal(t, len(st.sweepNew(true)), 2) // a dry run names them
	_, err = os.Stat(half)
	require.NoError(t, err)
	assert.Equal(t, len(st.sweepNew(false)), 2)
	for _, p := range []string{half, st.rootfs(id) + ".dl"} {
		_, err = os.Stat(p)
		assert.That(t, errors.Is(err, os.ErrNotExist), "the crashed unpack's %s is still there", p)
	}
}

// two pulls of one image at once converge on one rootfs, both done.
func TestBwrapStoreConcurrentPull(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	reg, ref := imageRegistry(t, nil, tarLayer(t, entry{name: "etc/os-release", body: "ubuntu"}))
	st, err := newBwrapStore()
	require.NoError(t, err)
	var wg sync.WaitGroup
	fetched := make([]bool, 4)
	errs := make([]error, 4)
	for i := range fetched {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ri, err := reg.image(context.Background(), ref, "amd64")
			if err != nil {
				errs[i] = err
				return
			}
			fetched[i], errs[i] = st.pull(ri, false, nil)
		}()
	}
	wg.Wait()
	n := 0
	for i := range fetched {
		require.NoError(t, errs[i])
		if fetched[i] {
			n++
		}
	}
	assert.Equal(t, n, 1)
	assert.Equal(t, len(st.ids()), 1)
}

// a session's lock dies with its process: a holder killed mid-run does not
// keep its image from going.
func TestBwrapStoreCrashedHolder(t *testing.T) {
	if os.Getenv("PATS_HOLD_LOCK") != "" {
		st, err := newBwrapStore()
		if err == nil {
			_, err = st.lock(os.Getenv("PATS_HOLD_LOCK"), false, false)
		}
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("held")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	st, err := newBwrapStore()
	require.NoError(t, err)
	id := "sha256:" + strings.Repeat("b", 64)
	require.NoError(t, os.MkdirAll(st.rootfs(id), 0o755))
	holder := exec.Command(os.Args[0], "-test.run", "^TestBwrapStoreCrashedHolder$")
	holder.Env = append(os.Environ(), "PATS_HOLD_LOCK="+id)
	out, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	line := make([]byte, 5)
	_, _ = io.ReadFull(out, line)
	require.Equal(t, string(line), "held\n")
	assert.Error(t, st.remove(id), "in use")
	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()
	require.NoError(t, st.remove(id))
	assert.Equal(t, st.has(id), false)
}

// a ref pinned by digest gets that digest or nothing; refs that differ never
// share a record.
func TestBwrapPinnedRef(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	reg, ref := imageRegistry(t, nil, tarLayer(t, entry{name: "a", body: "a"}))
	pinned := strings.TrimSuffix(ref, ":latest") + "@sha256:" + strings.Repeat("0", 64)
	_, err := reg.image(context.Background(), pinned, "amd64")
	assert.Error(t, err, "the registry answered with sha256:")
	st, err := newBwrapStore()
	require.NoError(t, err)
	assert.NotEqual(t, st.refFile("registry.example/team/a_b:latest"), st.refFile("registry.example/team_a/b:latest"))
}

// a ref record that is empty or not an id names nothing, never the store.
func TestBwrapRefRecordGarbage(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st, err := newBwrapStore()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(st.dir, "refs"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(st.dir, "rootfs"), 0o700))
	for _, body := range []string{"", "\n", "garbage\n"} {
		require.NoError(t, os.WriteFile(st.refFile("ghcr.io/x/y:1"), []byte(body), 0o600))
		_, _, ok := st.resolveRef("ghcr.io/x/y:1")
		assert.Equal(t, ok, false)
	}
}
