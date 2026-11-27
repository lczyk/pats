package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/version"
)

// the image cache is a directory of exported lxd images, a file each under
// its alias, which init imports from where the server lacks the alias
// (docs/spec/image-cache.md). pats only reads it.

// cacheExts are the endings lxc image export gives a unified image.
var cacheExts = []string{".tar", ".tar.gz", ".tar.zst", ".tar.xz", ".tar.bz2", ".tar.lzma"}

// errNoCached says the image cache has no file for an alias.
var errNoCached = errors.New("no file for it in the image cache")

// cacheName says whether the cache is asked for alias at all: one path
// element, so no name reaches a file outside the directory, and not hex
// digits alone, which may be the start of a fingerprint.
func cacheName(alias string) bool {
	return alias != "" && !strings.HasPrefix(alias, ".") && !strings.ContainsAny(alias, "/\\:\x00") &&
		filepath.IsLocal(alias) && !isHex(alias)
}

// cachedFile is the cache's file for alias, the newest of several.
func cachedFile(dir, alias string) (string, error) {
	var best string
	var mode fs.FileMode
	var newest time.Time
	for _, ext := range cacheExts {
		p := filepath.Join(dir, alias+ext)
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if best == "" || st.ModTime().After(newest) {
			best, mode, newest = p, st.Mode(), st.ModTime()
		}
	}
	switch {
	case best == "":
		return "", errNoCached
	case !mode.IsRegular():
		return "", fmt.Errorf("%s is not a regular file", best)
	}
	return best, nil
}

// fileSHA256 is the hex sha256 of a file: of a unified image's tarball, the
// fingerprint lxd gives it.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// builtAs checks an imported image is the one init would have built under
// key: what its publish said of it.
func builtAs(key string) func(props map[string]string) error {
	return func(props map[string]string) error {
		switch {
		case props["pats.inputs"] != key:
			return fmt.Errorf("it holds the image of key %q, not %s", props["pats.inputs"], key)
		case props["pats.version"] != version.Info.Version:
			return fmt.Errorf("it was built by pats %q, not %s", props["pats.version"], version.Info.Version)
		}
		return nil
	}
}

// cached says whether the image cache is asked for alias: a vm's image is
// a split one, two files, which the cache does not hold.
func (l *lxd) cached(alias string) bool {
	return l.o.ImageCache != "" && !l.o.VM && cacheName(alias)
}

// importCached brings alias in from the image cache and returns the
// fingerprint the alias then names: errNoCached when the cache has nothing
// for it, any other error when it has and the image could not be used.
// check, when set, sees the image's properties before the alias is made.
// failed is why the cache's file was not used when another init made the
// alias meanwhile and that image is returned.
func (l *lxd) importCached(ctx context.Context, alias string, check func(props map[string]string) error) (fp string, failed, err error) {
	if !l.cached(alias) {
		return "", nil, errNoCached
	}
	path, err := cachedFile(l.o.ImageCache, alias)
	if errors.Is(err, errNoCached) {
		return "", nil, err
	}
	if err == nil {
		if fp, err = l.importFile(ctx, path, alias, check); err == nil {
			return fp, nil, nil
		}
		err = fmt.Errorf("%s: %w", path, err)
	}
	err = fmt.Errorf("image cache: %w", err)
	theirs, aerr := l.fingerprint(ctx, alias)
	switch {
	case aerr == nil:
		return theirs, err, nil
	case !lxdMissing(aerr):
		// not known whether the alias is there: no build may follow.
		return "", nil, &aliasUnknown{err: err, check: aerr}
	}
	return "", nil, err
}

// aliasUnknown is a failed import whose check of the alias failed too.
type aliasUnknown struct{ err, check error }

func (e *aliasUnknown) Error() string {
	return fmt.Sprintf("%v; and the alias could not be checked: %v", e.err, e.check)
}

func (e *aliasUnknown) Unwrap() []error { return []error{e.err, e.check} }

// importFile imports a cache file when the server lacks its image, and
// makes alias on it. the alias is created, never set by the import: lxc
// image import --alias replaces one that is there.
func (l *lxd) importFile(ctx context.Context, path, alias string, check func(props map[string]string) error) (string, error) {
	fp, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if _, err := l.fingerprint(ctx, fp); err != nil {
		if !notFound(err) {
			return "", err
		}
		_, ierr := l.lxc(ctx, "image", "import", path, l.o.Remote+":", "--project", lxdProject)
		// a refused import is no failure yet: another init importing the
		// same file ends so, with the image there.
		if _, err := l.fingerprint(ctx, fp); err != nil {
			if ierr != nil {
				return "", ierr
			}
			return "", fmt.Errorf("the server did not take it as image %s: %w", fp[:12], err)
		}
	}
	if check != nil {
		props, err := l.imageProps(ctx, fp)
		if err != nil {
			return "", err
		}
		if err := check(props); err != nil {
			return "", err
		}
	}
	if _, err := l.lxc(ctx, "image", "alias", "create", l.ref(alias), fp, "--project", lxdProject); err != nil {
		// taken meanwhile: by this image or by another run's build.
		theirs, ferr := l.fingerprint(ctx, alias)
		if ferr != nil {
			return "", err
		}
		return theirs, nil
	}
	return fp, nil
}

// imageProps are the properties of the image with fingerprint fp.
func (l *lxd) imageProps(ctx context.Context, fp string) (map[string]string, error) {
	var im struct {
		Properties map[string]string `json:"properties"`
	}
	if err := l.query(ctx, "/1.0/images/"+fp+"?project="+lxdProject, &im); err != nil {
		return nil, err
	}
	return im.Properties, nil
}

// resolve is fingerprint, and for an alias the server lacks, the image
// cache: an image a pats.yaml names, made elsewhere.
func (l *lxd) resolve(ctx context.Context, ref string) (string, error) {
	fp, err := l.fingerprint(ctx, ref)
	if err == nil || !notFound(err) {
		return fp, err
	}
	cached, _, cerr := l.importCached(ctx, ref, nil)
	switch {
	case cerr == nil:
		return cached, nil
	case !errors.Is(cerr, errNoCached):
		return "", fmt.Errorf("%w (%w)", err, cerr)
	case l.cached(ref):
		return "", fmt.Errorf("%w (and no file for it in the image cache %s)", err, l.o.ImageCache)
	}
	return "", err
}
