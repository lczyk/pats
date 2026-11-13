package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// MkTemp creates a temp dir suitable for bind-mounting into a sandbox. it is
// NOT the system /tmp: a confined docker daemon (notably the docker snap) runs
// in its own mount namespace and can't see the host /tmp, so a `-v /tmp/...`
// silently mounts an empty dir instead of the files we staged. CacheDir
// lives under $HOME, which such daemons do share, hidden dirs included. the
// caller removes the dir.
//
// that is the daemon's view. the docker snap's cli sees less: no hidden dir
// at the top of the home, so not the cache. a dir the cli itself opens (a
// cidfile, a build context, a docker cp source) comes from MkTempFor.
//
// NOTE: if XDG_CACHE_HOME points outside $HOME this breaks the same way -- the
// fix then is to unset it. a fully remote daemon (no shared fs at all) can't be
// helped by any host path; that's out of scope.
//
// the dir gets a sibling <dir>.owner holding this process's run token, so
// `pats clean` can tell a live run's dir from a crashed one's by the run's
// lease. it is a sibling, not a file inside, because the dir is often
// bind-mounted as the agent's workdir.
func MkTemp(prefix string) (string, error) {
	return defaultProcess.mkTemp(prefix)
}

func (p *process) mkTemp(prefix string) (string, error) {
	if err := p.holdLease(); err != nil {
		return "", err
	}
	home, err := p.home()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dir+ownerSuffix, []byte(p.token), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// RemoveTemp removes a MkTemp dir and its owner file.
func RemoveTemp(dir string) error {
	err := removeTree(dir)
	_ = os.Remove(dir + ownerSuffix)
	if inCLIRoot(dir) {
		// the client's dir in the docker snap's data goes once empty.
		_ = os.Remove(filepath.Dir(dir))
	}
	return err
}

// ownerSuffix names the owner file beside each MkTemp dir.
const ownerSuffix = ".owner"

// processAlive asks the kernel whether pid exists: signal 0 delivers nothing
// but still checks. EPERM means it exists under another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TempRoot is the directory MkTemp creates its dirs in.
func TempRoot() (string, error) {
	cache, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "tmp"), nil
}

// removeTree removes p, making what is under it writable first: an unpacked
// image has read-only dirs, and overlayfs leaves its work dir mode 000.
func removeTree(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// a dir is visited before it is read, so its chmod comes first.
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}
