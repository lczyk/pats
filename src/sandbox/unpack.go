package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// what the tar readers share: a registry's layers (bwrap_image.go) and the
// tar an lxd instance writes (lxd.go) are both unpacked on the host, and
// neither archive is pats's own.

// errEscape is an archive entry that would land outside the directory it is
// unpacked into.
var errEscape = errors.New("leaves the destination")

// safeParent refuses a parent dir under root that is, or passes through, a
// symlink: an entry written there would land wherever the link points. root
// itself is the caller's, and is not looked at.
func safeParent(root, dir string) error {
	p := root
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755)
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("written through the symlink %s, which %w", strings.TrimPrefix(p, root), errEscape)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", strings.TrimPrefix(p, root))
		}
	}
	return nil
}
