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
	return s.push(ctx, hostPath, path, pushOpts{})
}

// pushOpts are how a bind's copy differs from a plain push.
type pushOpts struct {
	// replace empties path first, as a mount hides what the image has
	// there, and makes the dirs above path traversable for all.
	replace bool
	// ro makes the copy root's alone to change (roModes).
	ro bool
	// masks are in-box paths under path that arrive as empty dirs.
	masks []string
}

// pushBind copies b in as a mount would show it: over an emptied path, the
// masks empty, read-only ones root's, a symlinked source by what it names.
func (s *lxdSession) pushBind(ctx context.Context, b bind) error {
	src, err := filepath.EvalSymlinks(b.host)
	if err != nil {
		return err
	}
	return s.push(ctx, src, b.path, pushOpts{replace: true, ro: b.ro, masks: b.masks})
}

func (s *lxdSession) push(ctx context.Context, hostPath, path string, o pushOpts) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	var skip []string
	for _, m := range o.masks {
		if rel, err := filepath.Rel(path, m); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
			skip = append(skip, filepath.ToSlash(rel))
		}
	}
	pr, pw := io.Pipe()
	local := make(chan error, 1)
	go func() {
		// a file goes under the name it lands as: a symlinked source's own
		// name could be a sibling the image has.
		err := writeTar(pw, hostPath, fi, tarOpts{ro: o.ro, skip: skip, name: filepath.Base(path)})
		pw.CloseWithError(err)
		local <- err
	}()
	dest := path
	if !fi.IsDir() {
		dest = filepath.Dir(path)
	}
	// the file's name goes in as an argument, never as script text: it is
	// whatever the host path was called, and the script runs as root.
	script := ""
	if o.replace {
		// a dir above a bind keeps the image's mode: one shut to others
		// (/home/ubuntu 0750, /root 0700) would keep the agent user out.
		script = `d="$1"; while [ "$d" != / ] && [ "$d" != . ]; do [ -d "$d" ] && chmod a+x "$d"; d=$(dirname "$d"); done; `
	}
	script += `mkdir -p "$1"`
	if o.replace && fi.IsDir() {
		script += ` && rm -rf "$1"/* "$1"/.[!.]* "$1"/..?*`
	}
	script += ` && tar -x -m -C "$1"`
	if !fi.IsDir() {
		script += ` && { [ "$1/$3" = "$2" ] || mv "$1/$3" "$2"; }`
	}
	// the tar carries what is under a dir, not the dir: a read-only one is
	// made root's here, whatever the image had at its path.
	argv := []string{"sh", "-c", script, "push", dest, path, filepath.Base(path)}
	if o.ro && fi.IsDir() {
		argv[2] += ` && chown 0:0 "$1" && chmod "$4" "$1"`
		argv = append(argv, fmt.Sprintf("%o", roModes(int64(fi.Mode().Perm()), true)))
	}
	var out bytes.Buffer
	code, err := s.execIn(ctx, pr, Cmd{Argv: argv, Root: true, Dir: "/"}, &out, &out)
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

// tarOpts shape a pushed tree.
type tarOpts struct {
	ro   bool
	skip []string // slash paths, relative to the root, sent as empty dirs
	name string   // a file's entry name; "" is its own
}

// pushOpened runs once a pushed file is open and its size taken (a test
// changes the file there).
var pushOpened = func(string) {}

// ErrChanged is a file that shrank while its bytes were being pushed: its
// header promised bytes that are gone. a caller may push again.
var ErrChanged = errors.New("changed while it was pushed")

// roModes are a read-only copy's modes: no write for group or other, read
// for all, execute for all on a dir or where the owner has it, and no
// setuid or setgid, since the copy is root's.
func roModes(mode int64, dir bool) int64 {
	mode &^= 0o022 | 0o4000 | 0o2000
	mode |= 0o444
	if dir || mode&0o100 != 0 {
		mode |= 0o111
	}
	return mode
}

// writeTar writes hostPath (a dir's contents, or one file) as a tar stream.
// only directories, regular files and symlinks travel: a socket an agent or
// its tools left in the home (which archive/tar refuses), a fifo or a device
// node has no meaning on the other side. the tree may change while it goes:
// an entry gone before it is opened is left out, a file is sent at the size
// it has once open, so one that grows goes as a clean prefix, and only one
// that shrinks mid-copy fails, with ErrChanged.
func writeTar(w io.Writer, hostPath string, fi fs.FileInfo, o tarOpts) error {
	tw := tar.NewWriter(w)
	add := func(p, name string) error {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		var f *os.File
		if info.Mode().IsRegular() {
			if f, err = os.Open(p); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			defer f.Close()
			if info, err = f.Stat(); err != nil {
				return err
			}
			pushOpened(p)
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = name
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if o.ro && link == "" {
			h.Mode = roModes(h.Mode, info.IsDir())
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if f != nil {
			n, err := copyFileBytes(tw, io.LimitReader(f, h.Size))
			if err != nil {
				return err
			}
			if n < h.Size {
				return fmt.Errorf("%s: %w", p, ErrChanged)
			}
		}
		return nil
	}
	if !fi.IsDir() {
		name := o.name
		if name == "" {
			name = filepath.Base(hostPath)
		}
		if err := add(hostPath, name); err != nil {
			return err
		}
		return tw.Close()
	}
	masked := func(rel string) (self, under bool) {
		for _, m := range o.skip {
			if rel == m {
				return true, false
			}
			if strings.HasPrefix(rel, m+"/") {
				return false, true
			}
		}
		return false, false
	}
	err := filepath.WalkDir(hostPath, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(hostPath, p)
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if self, under := masked(name); under {
			return nil
		} else if self {
			// a mask arrives as an empty dir, whatever is there.
			h := &tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
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
