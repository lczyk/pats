package sandbox

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// copyInto copies src (a file, or a directory's contents) into dst, keeping
// modes and links. src and dst the same path is nothing to do.
func copyInto(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
		if err != nil {
			return err
		}
		return copyClose(out, in)
	}
	in, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyTree(in, dst)
}

// copyUnder is copyTree of the directory rel under base, a directory a
// sandbox wrote: rel is found inside base, whatever links lie on its way.
func copyUnder(base, rel, dst string) error {
	host, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer host.Close()
	in, err := host.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyTree(in, dst)
}

// copyTree copies the contents of the directory in into dst; dst that is
// that directory is nothing to do, and dst inside it is not copied into
// itself. both sides go through a root: what a sandbox left in the one it
// wrote is not followed out of it, and nothing is written past a link out of
// dst.
func copyTree(in *os.Root, dst string) error {
	top, err := in.Stat(".")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, top.Mode().Perm()|0o700); err != nil {
		return err
	}
	to, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if os.SameFile(top, to) {
		return nil
	}
	out, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	return copyDir(in, out, ".", to)
}

// copyDir copies the entries of the directory rel of in into the same of
// out, in name order, leaving out the directory skip. names are the
// filesystem's own bytes, text or not.
func copyDir(in, out *os.Root, rel string, skip fs.FileInfo) error {
	d, err := in.Open(rel)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		p := path.Join(rel, e.Name())
		if len(p) > maxPath {
			return fmt.Errorf("copy %.40s...: the name %w", p, errLong)
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			if os.SameFile(info, skip) {
				continue
			}
			if err := out.MkdirAll(p, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			if err := copyDir(in, out, p, skip); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := in.Readlink(p)
			if err != nil {
				return err
			}
			_ = out.Remove(p)
			if err := out.Symlink(link, p); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(in, out, p, info.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(in, out *os.Root, rel string, perm fs.FileMode) error {
	from, err := in.Open(rel)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := out.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	return copyClose(to, from)
}

// copyClose copies in to out and closes out.
func copyClose(out *os.File, in io.Reader) error {
	_, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
