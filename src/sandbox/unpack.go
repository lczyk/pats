package sandbox

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
)

// what the tar readers share: a registry's layers (bwrap_image.go) and the
// tar an lxd instance writes (lxd.go) are both unpacked on the host, and
// neither archive is pats's own. each is written through an os.Root of the
// directory it unpacks into, which follows a symlink only to where it stays
// inside that directory, so no entry is written through one that leaves it.

// errEscape is an archive entry whose own name or link names a place outside
// the directory it is unpacked into.
var errEscape = errors.New("leaves the destination")

// maxPath is the longest name an entry may have: linux's PATH_MAX. a root
// opens a path one component at a time and so takes one of any length, at a
// cost that grows with the square of its depth, and leaves a tree few tools
// can remove.
const maxPath = 4096

// copyBuf lends the buffer a file is copied through. io.Copy makes one of
// 32 KiB for every file it copies between a file and what is no file.
var copyBuf = sync.Pool{New: func() any { return new([32 << 10]byte) }}

// copyFileBytes is io.Copy through a lent buffer.
func copyFileBytes(dst io.Writer, src io.Reader) (int64, error) {
	buf := copyBuf.Get().(*[32 << 10]byte)
	defer copyBuf.Put(buf)
	// the bare interfaces: a file's own ReadFrom and WriteTo fall back to
	// io.Copy and its buffer.
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, buf[:])
}

var errLong = errors.New("is longer than a path may be")

// applyWhiteout removes what a whiteout in dir names: the opaque marker
// empties dir, any other names one entry. a dir that is not there has
// nothing to remove.
func applyWhiteout(root *os.Root, dir, base string) error {
	name := strings.TrimPrefix(base, ".wh.")
	if base != ".wh..wh..opq" {
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("a whiteout of %q %w", name, errEscape)
		}
		return root.RemoveAll(path.Join(dir, name))
	}
	d, err := root.Open(path.Join(dir, "."))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if errors.Is(err, syscall.ENOTDIR) {
		return nil // a file a lower layer left there has nothing in it
	}
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := root.RemoveAll(path.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

// unpackLayer applies one layer's tar to the rootfs in dir: whiteouts and
// opaque dirs remove what lower layers put there, device nodes are skipped
// (bwrap supplies /dev), setuid and setgid bits are dropped, ownership is not
// kept, and an entry whose path or hard link leaves the rootfs is refused.
// every entry is written through dir's root, so one that reaches past a
// symlink lands where that leads inside the rootfs, or is refused.
//
// whiteouts go first, in a pass of their own: they remove what lower layers
// put there, never what this layer adds, whatever the order in its tar.
func unpackLayer(open func() (io.ReadCloser, error), dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, whiteouts := range []bool{true, false} {
		r, err := open()
		if err != nil {
			return err
		}
		err = applyLayer(r, root, whiteouts)
		r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// applyLayer is one pass of unpackLayer: the whiteouts alone, or the rest.
func applyLayer(r io.Reader, root *os.Root, whiteouts bool) error {
	tr := tar.NewReader(r)
	// the directories this pass knows are there, so each is looked for once:
	// a root walks a path anew on every call. forgotten when an entry takes
	// the place of another, which may have been one of them.
	made := map[string]bool{}
	replace := func(rel string) error {
		clear(made)
		return root.RemoveAll(rel)
	}
	// the whiteouts pass: the whiteouts, and the directories the layer
	// brings, which say where a whiteout has nothing below to remove.
	var whs [][3]string // an entry's name, its dir and its base
	dirs := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			for _, wh := range whs {
				if ownDir(root, dirs, wh[1]) {
					continue
				}
				if err := applyWhiteout(root, wh[1], wh[2]); err != nil {
					return fmt.Errorf("%s: %w", wh[0], err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := entryPath(h.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the root itself
		}
		dir, base := path.Split(rel)
		wh := strings.HasPrefix(base, ".wh.")
		if whiteouts {
			switch {
			case wh:
				whs = append(whs, [3]string{h.Name, dir, base})
			case h.Typeflag == tar.TypeDir:
				dirs[rel] = true
			}
			continue
		}
		if wh {
			continue
		}
		if parent := path.Dir(rel); !made[parent] {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			made[parent] = true
		}
		mode := fs.FileMode(h.Mode).Perm() // setuid and setgid dropped
		if h.Mode&01000 != 0 {
			mode |= fs.ModeSticky
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err := root.Mkdir(rel, 0o700)
			if errors.Is(err, fs.ErrExist) {
				// a directory already there is kept; anything else, a link
				// to one too, gives way.
				if fi, lerr := root.Lstat(rel); lerr == nil && fi.IsDir() {
					err = nil
				} else if err = replace(rel); err == nil {
					err = root.Mkdir(rel, 0o700)
				}
			}
			if err != nil {
				return err
			}
			// the owner (pats's user) must be able to fill it.
			if err := root.Chmod(rel, mode|0o700); err != nil {
				return err
			}
			made[rel] = true
		case tar.TypeReg:
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm()|0o600)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					f, err = root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm()|0o600)
				}
			}
			if err != nil {
				return err
			}
			_, err = copyFileBytes(f, tr)
			if err == nil && mode&fs.ModeSticky != 0 {
				// a root's OpenFile takes the nine bits only: the tenth is
				// added to the ones the umask left.
				var fi fs.FileInfo
				if fi, err = f.Stat(); err == nil {
					err = f.Chmod(fi.Mode().Perm() | fs.ModeSticky)
				}
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			// how far a link may climb is counted from where its path
			// says it is, which a link on that path would move.
			if linkEscapes(dir, h.Linkname) || (strings.Contains(h.Linkname, "..") && throughLink(root, dir)) {
				return fmt.Errorf("%s: symlink to %s %w", h.Name, h.Linkname, errEscape)
			}
			err := root.Symlink(h.Linkname, rel)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					err = root.Symlink(h.Linkname, rel)
				}
			}
			if err != nil {
				return err
			}
		case tar.TypeLink:
			src, err := entryPath(h.Linkname)
			if err != nil || src == "" {
				return fmt.Errorf("%s: hard link to %s %w", h.Name, h.Linkname, errEscape)
			}
			if src == rel {
				break // a name linked to itself is the entry it already is
			}
			err = root.Link(src, rel)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					err = root.Link(src, rel)
				}
			}
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
		default:
			// devices, fifos and the like: bwrap supplies /dev.
		}
	}
}

// entryPath is a tar entry's path relative to the rootfs, "" for the root;
// one with a .. component is refused, never clamped.
func entryPath(name string) (string, error) {
	if len(name) > maxPath {
		return "", fmt.Errorf("%.40s...: %w", name, errLong)
	}
	n := strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/")
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("%s: %w", name, errEscape)
		}
	}
	n = path.Clean("/" + n)
	return strings.TrimPrefix(n, "/"), nil
}

// ownDir says whether dir lies in a directory the layer brings (dirs) where
// the rootfs holds something else, a link to a directory too. the layer's
// own directory takes that place, with nothing of the lower layers in it:
// a whiteout there has nothing to remove, and followed through the link
// would remove what the link points at.
func ownDir(root *os.Root, dirs map[string]bool, dir string) bool {
	p := ""
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		p = path.Join(p, part)
		if !dirs[p] {
			continue
		}
		if fi, err := root.Lstat(p); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// throughLink says whether a component of dir is a symlink.
func throughLink(root *os.Root, dir string) bool {
	p := ""
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		p = path.Join(p, part)
		if fi, err := root.Lstat(p); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// linkEscapes says whether a symlink in dir pointing at target climbs out of
// the rootfs. an absolute target resolves inside the rootfs at run time.
func linkEscapes(dir, target string) bool {
	if path.IsAbs(target) {
		return false
	}
	depth := 0
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part != "" {
			depth++
		}
	}
	for _, part := range strings.Split(target, "/") {
		switch part {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}
