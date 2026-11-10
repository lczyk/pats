package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Push streams hostPath into path as a tar over exec. a directory's contents
// land in path; a file lands at path.
func (s *lxdSession) Push(ctx context.Context, hostPath, path string) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	local := make(chan error, 1)
	go func() {
		err := writeTar(pw, hostPath, fi)
		pw.CloseWithError(err)
		local <- err
	}()
	dest := path
	if !fi.IsDir() {
		dest = filepath.Dir(path)
	}
	// the file's name goes in as an argument, never as script text: it is
	// whatever the host path was called, and the script runs as root.
	script := `mkdir -p "$1" && tar -x -m -C "$1"`
	if !fi.IsDir() {
		script += ` && { [ "$1/$3" = "$2" ] || mv "$1/$3" "$2"; }`
	}
	var out bytes.Buffer
	code, err := s.execIn(ctx, pr, Cmd{Argv: []string{"sh", "-c", script, "push", dest, path, filepath.Base(hostPath)}, Root: true, Dir: "/"}, &out, &out)
	pr.Close()
	// the local side failing is what made the remote tar complain, so it
	// leads the error.
	if lerr := <-local; lerr != nil && !errors.Is(lerr, io.ErrClosedPipe) {
		return fmt.Errorf("push %s: %w (in the instance: %s)", hostPath, lerr, strings.TrimSpace(out.String()))
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("push %s: %w", hostPath, err)
	}
	return nil
}

// writeTar writes hostPath (a dir's contents, or one file) as a tar stream.
// only directories, regular files and symlinks travel: a socket an agent or
// its tools left in the home (which archive/tar refuses), a fifo or a device
// node has no meaning on the other side.
func writeTar(w io.Writer, hostPath string, fi fs.FileInfo) error {
	tw := tar.NewWriter(w)
	add := func(p, name string) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = name
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = copyFileBytes(tw, f)
			f.Close()
			return err
		}
		return nil
	}
	if !fi.IsDir() {
		if err := add(hostPath, filepath.Base(hostPath)); err != nil {
			return err
		}
		return tw.Close()
	}
	err := filepath.WalkDir(hostPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(hostPath, p)
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			name += "/"
		}
		return add(p, name)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Pull streams path out as a tar over exec into hostDir: a directory's
// contents land in hostDir.
func (s *lxdSession) Pull(ctx context.Context, path, hostDir string) error {
	pr, pw := io.Pipe()
	var errb bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := s.execIn(ctx, nil, Cmd{Argv: []string{"tar", "-c", "-C", path, "."}, Root: true, Dir: "/"}, pw, &errb)
		if err == nil && code != 0 {
			err = fmt.Errorf("tar exited %d: %s", code, strings.TrimSpace(errb.String()))
		}
		pw.CloseWithError(err)
		done <- err
	}()
	err := readTar(pr, hostDir)
	// tar's trailing padding follows the end-of-archive marker readTar
	// stops at; drain it so the writer does not fail on a closed pipe.
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	if perr := <-done; perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return fmt.Errorf("pull %s: %w", path, err)
	}
	return nil
}

// readTar extracts a tar stream into dir as the current user. the stream is
// the word of the tar inside an instance, not pats's own: a name or a hard
// link that leaves dir is refused, and every entry is written through dir's
// root, so nothing lands past a symlink that leaves dir (dir itself may be
// reached through one). symlinks are made last, so only another symlink can
// meet one the stream made.
func readTar(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	links := map[string]string{} // a symlink's path -> its target
	skipped := map[string]bool{} // what does not travel: fifos, devices
	files := map[string]bool{}   // the regular files this stream made
	// the directories known to be there, so each is looked for once: a root
	// walks a path anew on every call. forgotten when an entry is removed,
	// which may have been one of them.
	made := map[string]bool{}
	parent := func(rel string) error {
		dir := path.Dir(rel)
		if made[dir] {
			return nil
		}
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		made[dir] = true
		return nil
	}
	remove := func(rel string) {
		clear(made)
		_ = root.Remove(rel)
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, err := tarRel(h.Name)
		if err != nil {
			return err
		}
		switch {
		case rel == ".":
			continue
		case h.Typeflag == tar.TypeSymlink:
			links[rel] = h.Linkname
			delete(skipped, rel)
			delete(files, rel)
			continue
		case h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeLink:
			skipped[rel] = true
			delete(links, rel)
			delete(files, rel)
			continue
		}
		// tar names a file's second path by its first, which came before it.
		// a name linked to itself is the entry it already is.
		src := ""
		if h.Typeflag == tar.TypeLink {
			if src, err = tarRel(h.Linkname); err != nil || src == "." {
				return fmt.Errorf("tar entry %q: hard link to %q %w", h.Name, h.Linkname, errEscape)
			}
			if src == rel {
				continue
			}
		}
		// a later entry of a name replaces an earlier one, as tar unpacks.
		delete(links, rel)
		delete(skipped, rel)
		delete(files, rel)
		if err := parent(rel); err != nil {
			return fmt.Errorf("tar entry %q: %w", h.Name, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err := root.Mkdir(rel, 0o755)
			if errors.Is(err, fs.ErrExist) {
				// a directory already there is kept. a link or a file gives
				// way: Chmod would follow a link to the directory it names.
				if fi, lerr := root.Lstat(rel); lerr == nil && fi.IsDir() {
					err = nil
				} else if err = root.Remove(rel); err == nil {
					clear(made)
					err = root.Mkdir(rel, 0o755)
				}
			}
			if err != nil {
				return err
			}
			_ = root.Chmod(rel, fs.FileMode(h.Mode).Perm()|0o700)
			made[rel] = true
		case tar.TypeReg:
			perm := fs.FileMode(h.Mode).Perm() | 0o600
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			if errors.Is(err, fs.ErrExist) {
				remove(rel)
				f, err = root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			}
			if err != nil {
				return err
			}
			_, err = copyFileBytes(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			files[rel] = true
		case tar.TypeLink:
			// the second name of a symlink is that symlink again, and of
			// what does not travel, left out with it.
			if target, isLink := links[src]; isLink {
				links[rel] = target
				continue
			}
			if skipped[src] {
				skipped[rel] = true
				continue
			}
			// only to a file this stream made: one that was there before
			// may be another name of a file outside dir.
			if !files[src] {
				return fmt.Errorf("tar entry %q: hard link to %q, which is no file of this archive", h.Name, h.Linkname)
			}
			err := root.Link(src, rel)
			if errors.Is(err, fs.ErrExist) {
				remove(rel)
				err = root.Link(src, rel)
			}
			if err != nil {
				return err
			}
			files[rel] = true
		}
	}
	rels := make([]string, 0, len(links))
	for rel := range links {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := parent(rel); err != nil {
			return fmt.Errorf("tar entry %q: %w", rel, err)
		}
		err := root.Symlink(links[rel], rel)
		if errors.Is(err, fs.ErrExist) {
			remove(rel)
			err = root.Symlink(links[rel], rel)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// tarRel is a tar entry's name as a clean path relative to where it is
// unpacked, "." for that directory itself; a name that leaves it is refused.
func tarRel(name string) (string, error) {
	if len(name) > maxPath {
		return "", fmt.Errorf("tar entry %.40q... %w", name, errLong)
	}
	rel := path.Clean(filepath.ToSlash(name))
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("tar entry %q %w", name, errEscape)
	}
	return rel, nil
}

// Sync pulls every writable bind back to its host path, replacing what was
// there, so the host sees what the agent left: collect reads it. a bind is
// pulled whole beside its host path first and swapped in only then, so a
// pull that fails (a deadline, a server gone, a name readTar refuses) leaves
// the host copy as it was.
func (s *lxdSession) Sync(ctx context.Context) error {
	for _, b := range s.binds {
		if b.ro {
			continue
		}
		if err := s.syncBind(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *lxdSession) syncBind(ctx context.Context, b bind) error {
	fresh, err := os.MkdirTemp(filepath.Dir(b.host), "."+filepath.Base(b.host)+".sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(fresh)
	if err := s.Pull(ctx, b.path, fresh); err != nil {
		return err
	}
	if err := clearDir(b.host); err != nil {
		return err
	}
	ents, err := os.ReadDir(fresh)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.Rename(filepath.Join(fresh, e.Name()), filepath.Join(b.host, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// clearDir empties dir, keeping dir itself.
func clearDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
