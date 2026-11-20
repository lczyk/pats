package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/version"
)

// cacheFile writes an exported image into dir under name: a tar with the
// metadata.yaml an import reads its properties from. body sets its bytes
// apart from another's, and so its fingerprint.
func cacheFile(t *testing.T, dir, name, body string, props map[string]string) (path, fp string) {
	t.Helper()
	var meta strings.Builder
	meta.WriteString("architecture: x86_64\nproperties:\n")
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		meta.WriteString("  " + k + ": \"" + props[k] + "\"\n")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, data string }{{"metadata.yaml", meta.String()}, {"rootfs/id", body}} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data))}))
		_, err := tw.Write([]byte(f.data))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	path = filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	sum := sha256.Sum256(buf.Bytes())
	return path, hex.EncodeToString(sum[:])
}

// builtProps are the properties a publish gives the image of key.
func builtProps(key string) map[string]string {
	return map[string]string{"pats.inputs": key, "pats.version": version.Info.Version, "pats.stamped": "1"}
}

// keyOf is the key spec's image has on a server whose base is fakeFP(spec.Base).
func keyOf(t *testing.T, spec lxdImageSpec) string {
	t.Helper()
	key, err := imageKey(fakeFP(spec.Base), spec, false)
	require.NoError(t, err)
	return key
}

// calls are f's calls that start with prefix.
func calls(f *fakeServer, prefix string) (out []string) {
	for _, c := range f.all() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func listing(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range ents {
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out = append(out, e.Name()+" "+info.Mode().String()+" "+info.ModTime().String())
	}
	return strings.Join(out, "\n")
}

// an image init would build comes from the cache when its file is there:
// imported with no alias, the alias created on the file's hash, nothing
// built, and the directory as it was.
func TestLXDImageCacheHit(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	dir := t.TempDir()
	path, fp := cacheFile(t, dir, "pats-img-"+key+".tar.zst", "one", builtProps(key))
	before := listing(t, dir)

	f := newFakeServer(t)
	l := lxdOn(t, f, Options{ImageCache: dir})
	var steps, warns []string
	spec.Progress, spec.Warn = func(m string) { steps = append(steps, m) }, func(m string) { warns = append(warns, m) }
	alias, got, _, built, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, alias, "pats-img-"+key)
	assert.Equal(t, got, fp)
	assert.That(t, !built, "a cache hit built")
	assert.Equal(t, f.lookupImage(alias), fp)
	assert.Equal(t, f.images[fp].props["pats.stamped"], "1") // the image's own properties came with it
	assert.Len(t, calls(f, "launch"), 0)
	assert.Len(t, calls(f, "publish"), 0)
	require.Len(t, calls(f, "image import"), 1)
	assert.ContainsString(t, calls(f, "image import")[0], "image import "+path+" srv: ")
	assert.Len(t, calls(f, "image alias create srv:"+alias+" "+fp), 1)
	for _, im := range f.images {
		for _, a := range im.aliases {
			assert.That(t, !runPrivateAlias(a), "an import left a run-private alias: %s", a)
		}
	}
	assert.Len(t, warns, 0)
	assert.ContainsString(t, strings.Join(steps, "\n"), "imported lxd image "+alias)
	assert.Equal(t, listing(t, dir), before)

	// the server has the alias now: the directory is not asked again.
	f.reset()
	_, again, _, _, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, again, fp)
	assert.Len(t, calls(f, "image import"), 0)
}

// what is not a usable file for the key is a warning that names it and a
// build, and the directory is left as it was. no file at all is no warning.
func TestLXDImageCacheMiss(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	name := "pats-img-" + key + ".tar.gz"
	other := lxdImageSpec{Base: "img", Argv: []string{"false"}}
	for _, c := range []struct {
		name string
		make func(t *testing.T, dir string)
		warn string // "" for none
		left int    // images the server holds beside the built one
	}{
		{"no file", func(*testing.T, string) {}, "", 0},
		{"another key's file", func(t *testing.T, dir string) {
			k := keyOf(t, other)
			cacheFile(t, dir, "pats-img-"+k+".tar.gz", "x", builtProps(k))
		}, "", 0},
		{"damaged", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("not an image"), 0o644))
		}, name, 0},
		{"a symlink", func(t *testing.T, dir string) {
			real, _ := cacheFile(t, t.TempDir(), "elsewhere.tar.gz", "x", builtProps(key))
			require.NoError(t, os.Symlink(real, filepath.Join(dir, name)))
		}, "not a regular file", 0},
		{"a directory", func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
		}, "not a regular file", 0},
		{"a fifo", func(t *testing.T, dir string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(dir, name), 0o644))
		}, "not a regular file", 0},
		{"another key's image under the name", func(t *testing.T, dir string) {
			cacheFile(t, dir, name, "x", builtProps(keyOf(t, other)))
		}, "not " + key, 1},
		{"another pats's image under the name", func(t *testing.T, dir string) {
			p := builtProps(key)
			p["pats.version"] = "0.0.1"
			cacheFile(t, dir, name, "x", p)
		}, `built by pats "0.0.1"`, 1},
		{"a split image's two files", func(t *testing.T, dir string) {
			cacheFile(t, dir, "pats-img-"+key, "x", builtProps(key))
			cacheFile(t, dir, "pats-img-"+key+".squashfs", "x", builtProps(key))
		}, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.make(t, dir)
			before := listing(t, dir)
			f := newFakeServer(t)
			l := lxdOn(t, f, Options{ImageCache: dir})
			var warns []string
			s := spec
			s.Warn = func(m string) { warns = append(warns, m) }
			alias, fp, _, built, err := l.image(context.Background(), s, io.Discard)
			require.NoError(t, err)
			assert.That(t, built, "nothing was built")
			assert.Equal(t, f.lookupImage(alias), fp)
			if c.warn == "" {
				assert.Len(t, warns, 0)
			} else {
				require.Len(t, warns, 1)
				assert.ContainsString(t, warns[0], c.warn)
				assert.ContainsString(t, warns[0], name) // the file is named
			}
			// the base, the built image, and what an import left with no alias.
			unnamed := 0
			for id, im := range f.images {
				if id != fp && len(im.aliases) == 0 && id != fakeFP("img") {
					unnamed++
				}
			}
			assert.Equal(t, unnamed, c.left)
			assert.Equal(t, listing(t, dir), before)
		})
	}
}

// an image the server already has under the alias is used, and the
// directory is not read; --force builds with a file there, and imports
// nothing.
func TestLXDImageCacheNotAsked(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	dir := t.TempDir()
	cacheFile(t, dir, "pats-img-"+key+".tar", "cached", builtProps(key))

	f := newFakeServer(t)
	plain := lxdOn(t, f, Options{})
	_, built, _, _, err := plain.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	f.reset()
	l := lxdOn(t, f, Options{ImageCache: dir})
	_, fp, _, _, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, fp, built)
	assert.Len(t, calls(f, "image import"), 0)

	forced := spec
	forced.Force = true
	f.reset()
	_, fp, _, wasBuilt, err := l.image(context.Background(), forced, io.Discard)
	require.NoError(t, err)
	assert.That(t, wasBuilt && fp != built, "a forced build did not build")
	assert.Len(t, calls(f, "image import"), 0)
	assert.Len(t, calls(f, "publish"), 1)
}

// an import that did not get as far as its alias, and two inits on one
// file: the image the server has is given the alias, and nothing is built.
func TestLXDImageCacheServerHasImage(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	alias := "pats-img-" + key
	t.Run("cut short before its alias", func(t *testing.T) {
		dir := t.TempDir()
		_, fp := cacheFile(t, dir, alias+".tar", "one", builtProps(key))
		f := newFakeServer(t)
		f.images[fp] = &fakeImage{props: builtProps(key)}
		l := lxdOn(t, f, Options{ImageCache: dir})
		_, got, _, built, err := l.image(context.Background(), spec, io.Discard)
		require.NoError(t, err)
		assert.That(t, !built && got == fp, "built %v, fingerprint %s", built, got)
		assert.Len(t, calls(f, "image import"), 0)
		assert.Equal(t, f.lookupImage(alias), fp)
	})
	t.Run("another init imports it first", func(t *testing.T) {
		dir := t.TempDir()
		_, fp := cacheFile(t, dir, alias+".tar", "one", builtProps(key))
		f := newFakeServer(t)
		f.importSeen = func(fp string) { f.images[fp] = &fakeImage{props: builtProps(key)} }
		l := lxdOn(t, f, Options{ImageCache: dir})
		var warns []string
		s := spec
		s.Warn = func(m string) { warns = append(warns, m) }
		_, got, _, built, err := l.image(context.Background(), s, io.Discard)
		require.NoError(t, err)
		assert.That(t, !built && got == fp, "built %v, fingerprint %s", built, got)
		assert.Len(t, calls(f, "image import"), 1) // refused, and no failure
		assert.Len(t, warns, 0)
		assert.Equal(t, f.lookupImage(alias), fp)
	})
	t.Run("the alias is another build's by the claim", func(t *testing.T) {
		dir := t.TempDir()
		_, fp := cacheFile(t, dir, alias+".tar", "one", builtProps(key))
		f := newFakeServer(t)
		theirs := fakeFP("theirs")
		f.before = func(args []string) {
			if len(args) > 2 && args[0] == "image" && args[1] == "alias" && args[2] == "create" {
				f.images[theirs] = &fakeImage{aliases: []string{alias}}
			}
		}
		l := lxdOn(t, f, Options{ImageCache: dir})
		_, got, _, built, err := l.image(context.Background(), spec, io.Discard)
		require.NoError(t, err)
		assert.That(t, !built && got == theirs, "built %v, fingerprint %s", built, got)
		assert.Equal(t, f.lookupImage(alias), theirs) // never re-pointed
		assert.Len(t, f.images[fp].aliases, 0)        // the imported one is left with none
	})
	t.Run("the alias is made while the import fails", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, alias+".tar"), []byte("damaged"), 0o644))
		f := newFakeServer(t)
		theirs := fakeFP("theirs")
		f.before = func(args []string) {
			if len(args) > 1 && args[0] == "image" && args[1] == "import" {
				f.images[theirs] = &fakeImage{aliases: []string{alias}}
			}
		}
		l := lxdOn(t, f, Options{ImageCache: dir})
		var warns []string
		s := spec
		s.Warn = func(m string) { warns = append(warns, m) }
		_, got, _, built, err := l.image(context.Background(), s, io.Discard)
		require.NoError(t, err)
		assert.That(t, !built && got == theirs, "built %v, fingerprint %s", built, got)
		// the file that could not be used is still said.
		require.Len(t, warns, 1)
		assert.ContainsString(t, warns[0], alias+".tar")
	})
}

// a vm's image is a split one, which the cache does not hold: a vm sandbox
// never asks the directory, for an image it builds or one it is given.
func TestLXDImageCacheNotForVMs(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key, err := imageKey(fakeFP("img"), spec, true)
	require.NoError(t, err)
	dir := t.TempDir()
	cacheFile(t, dir, "pats-img-"+key+".tar", "one", builtProps(key))
	cacheFile(t, dir, "my-rock.tar", "hand-made", nil)
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{ImageCache: dir, VM: true})
	_, _, _, built, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.That(t, built, "a vm's image came from the cache")
	_, err = l.Resolve(context.Background(), "my-rock")
	assert.ErrorIs(t, err, ErrNoImage)
	assert.That(t, !strings.Contains(err.Error(), "image cache"), "the cache named for a vm: %v", err)
	assert.Len(t, calls(f, "image import"), 0)
}

// two files for one alias: the newer is the one used.
func TestLXDImageCacheNewestFile(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	dir := t.TempDir()
	older, _ := cacheFile(t, dir, "pats-img-"+key+".tar.gz", "older", builtProps(key))
	_, newer := cacheFile(t, dir, "pats-img-"+key+".tar.zst", "newer", builtProps(key))
	require.NoError(t, os.Chtimes(older, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	f := newFakeServer(t)
	_, fp, _, _, err := lxdOn(t, f, Options{ImageCache: dir}).image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, fp, newer)
}

// the upper layer's key is made of the lower one's fingerprint, which an
// import keeps: both come from the cache on a new server.
func TestLXDImageCacheLayers(t *testing.T) {
	lower := lxdImageSpec{Base: "img", Argv: []string{"lower"}}
	lowKey := keyOf(t, lower)
	dir := t.TempDir()
	_, lowFP := cacheFile(t, dir, "pats-img-"+lowKey+".tar.zst", "lower", builtProps(lowKey))
	upper := lxdImageSpec{Base: "pats-img-" + lowKey, Argv: []string{"upper"}}
	upKey, err := imageKey(lowFP, upper, false)
	require.NoError(t, err)
	_, upFP := cacheFile(t, dir, "pats-img-"+upKey+".tar.zst", "upper", builtProps(upKey))

	f := newFakeServer(t)
	l := lxdOn(t, f, Options{ImageCache: dir})
	_, fp, _, built, err := l.image(context.Background(), lower, io.Discard)
	require.NoError(t, err)
	assert.That(t, !built && fp == lowFP, "the lower layer: built %v, %s", built, fp)
	_, fp, base, built, err := l.image(context.Background(), upper, io.Discard)
	require.NoError(t, err)
	assert.That(t, !built && fp == upFP, "the upper layer: built %v, %s", built, fp)
	assert.Equal(t, base, lowFP)
	assert.Len(t, calls(f, "launch"), 0)
}

func TestCacheName(t *testing.T) {
	for alias, want := range map[string]bool{
		"my-rock": true, "pats-img-0123456789abcdef01234567": true, "ubuntu-24.04": true, "rock_v2": true,
		"": false, ".": false, "..": false, ".hidden": false, "../x": false, "a/b": false, `a\b`: false,
		"/abs": false, "ubuntu:26.04": false, "abc123": false, "0123456789abcdef": false, "nul\x00": false,
	} {
		assert.Equal(t, cacheName(alias), want, alias)
	}
}

// an image a pats.yaml names, which the server lacks, comes from the cache
// under its own alias, unchecked; one the server has is not looked for, and
// nor is a name that is no alias for a file.
func TestLXDImageCacheNamedImage(t *testing.T) {
	ctx := context.Background()
	t.Run("imported under its alias", func(t *testing.T) {
		dir := t.TempDir()
		_, fp := cacheFile(t, dir, "my-rock.tar.zst", "hand-made", nil)
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{ImageCache: dir})
		img, err := l.Resolve(ctx, "my-rock")
		require.NoError(t, err)
		assert.Equal(t, img.ID, fp)
		assert.Equal(t, f.lookupImage("my-rock"), fp)
		assert.Len(t, calls(f, "launch"), 0)
	})
	t.Run("a pats-img name a pats.yaml gives is not checked", func(t *testing.T) {
		dir := t.TempDir()
		name := "pats-img-0123456789abcdef01234567"
		_, fp := cacheFile(t, dir, name+".tar", "hand-made", nil)
		f := newFakeServer(t)
		img, err := lxdOn(t, f, Options{ImageCache: dir}).Resolve(ctx, name)
		require.NoError(t, err)
		assert.Equal(t, img.ID, fp)
	})
	t.Run("another init imports it first", func(t *testing.T) {
		dir := t.TempDir()
		_, fp := cacheFile(t, dir, "my-rock.tar", "hand-made", nil)
		f := newFakeServer(t)
		f.importSeen = func(fp string) { f.images[fp] = &fakeImage{props: map[string]string{}} }
		img, err := lxdOn(t, f, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		require.NoError(t, err) // a refused import of bytes the server has is no failure
		assert.Equal(t, img.ID, fp)
		assert.Equal(t, f.lookupImage("my-rock"), fp)
	})
	t.Run("a replaced file is another image on a new server", func(t *testing.T) {
		dir := t.TempDir()
		_, first := cacheFile(t, dir, "my-rock.tar", "first", nil)
		old := newFakeServer(t)
		img, err := lxdOn(t, old, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		require.NoError(t, err)
		assert.Equal(t, img.ID, first)
		_, second := cacheFile(t, dir, "my-rock.tar", "second", nil)
		// the server that has the alias keeps its image; a new one takes the file.
		img, err = lxdOn(t, old, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		require.NoError(t, err)
		assert.Equal(t, img.ID, first)
		img, err = lxdOn(t, newFakeServer(t), Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		require.NoError(t, err)
		assert.Equal(t, img.ID, second)
	})
	t.Run("the server's alias wins", func(t *testing.T) {
		dir := t.TempDir()
		cacheFile(t, dir, "my-rock.tar.zst", "newer", nil)
		f := newFakeServer(t)
		f.images[fakeFP("mine")] = &fakeImage{aliases: []string{"my-rock"}}
		img, err := lxdOn(t, f, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		require.NoError(t, err)
		assert.Equal(t, img.ID, fakeFP("mine"))
		assert.Len(t, calls(f, "image import"), 0)
	})
	t.Run("neither", func(t *testing.T) {
		dir := t.TempDir()
		f := newFakeServer(t)
		_, err := lxdOn(t, f, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		assert.ErrorIs(t, err, ErrNoImage)
		assert.ContainsString(t, err.Error(), "no file for it in the image cache "+dir)
		_, err = lxdOn(t, f, Options{}).Resolve(ctx, "my-rock")
		assert.ErrorIs(t, err, ErrNoImage)
		assert.That(t, !strings.Contains(err.Error(), "image cache"), "the cache named with no flag: %v", err)
	})
	t.Run("a damaged file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "my-rock.tar"), []byte("damaged"), 0o644))
		f := newFakeServer(t)
		_, err := lxdOn(t, f, Options{ImageCache: dir}).Resolve(ctx, "my-rock")
		assert.ErrorIs(t, err, ErrNoImage)
		assert.ContainsString(t, err.Error(), filepath.Join(dir, "my-rock.tar"))
	})
	t.Run("names that have no file", func(t *testing.T) {
		outside := t.TempDir()
		dir := filepath.Join(outside, "cache")
		require.NoError(t, os.Mkdir(dir, 0o755))
		cacheFile(t, outside, "x.tar", "outside", nil)
		cacheFile(t, dir, "abc123.tar", "hex", nil)
		require.NoError(t, os.Mkdir(filepath.Join(dir, "a"), 0o755))
		cacheFile(t, filepath.Join(dir, "a"), "b.tar", "nested", nil)
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{ImageCache: dir})
		// the beginning of two fingerprints: the error it is with no flag.
		f.images["abc1"+strings.Repeat("0", 60)] = &fakeImage{}
		f.images["abc1"+strings.Repeat("1", 60)] = &fakeImage{}
		cacheFile(t, dir, "abc1.tar", "hex", nil)
		for _, ref := range []string{"../x", "a/b", "abc123", "abc1", strings.Repeat("ab", 32)} {
			_, err := l.Resolve(ctx, ref)
			assert.Error(t, err, "")
			assert.That(t, !strings.Contains(err.Error(), "image cache"), "%s: the cache was asked: %v", ref, err)
		}
		assert.Len(t, calls(f, "image import"), 0)
		assert.Equal(t, f.lookupImage("abc1"), "")
	})
}

// a base a pats.yaml names comes from the cache as an image: does, and is
// then built on, with its fingerprint in the key. forced, it still does:
// the alias is the user's image, not one pats rebuilds.
func TestLXDImageCacheBase(t *testing.T) {
	for _, force := range []bool{false, true} {
		dir := t.TempDir()
		_, baseFP := cacheFile(t, dir, "my-base.tar.zst", "hand-made", nil)
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{ImageCache: dir})
		spec := lxdImageSpec{Base: "my-base", Argv: []string{"true"}, Force: force}
		alias, _, base, built, err := l.image(context.Background(), spec, io.Discard)
		require.NoError(t, err)
		assert.That(t, built, "nothing was built on the base")
		assert.Equal(t, base, baseFP)
		assert.Equal(t, f.lookupImage("my-base"), baseFP)
		want, err := imageKey(baseFP, spec, false)
		require.NoError(t, err)
		assert.Equal(t, alias, "pats-img-"+want)
		assert.ContainsString(t, strings.Join(f.call("launch"), " "), "launch srv:"+baseFP+" ")
	}
}
