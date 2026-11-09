package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CacheDir is pats's per-user state dir on every os: $XDG_CACHE_HOME/pats
// when that is set and absolute (a relative one is ignored, as the xdg spec
// says), else ~/.cache/pats. it holds the temp root, the lease files, the
// client id and init's projects registry. see docs/spec/cache-dir.md.
func CacheDir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "pats"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "pats"), nil
}

// legacyCacheDir is where a pats before CacheDir kept its temp root, leases
// and client id (os.UserCacheDir: ~/Library/Caches on macos), when that is
// another directory than CacheDir; "" when there is none.
func legacyCacheDir() string {
	old, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	old = filepath.Join(old, "pats")
	cur, err := CacheDir()
	if err != nil || resolvePath(old) == resolvePath(cur) {
		return ""
	}
	return old
}

// resolvePath is p with symlinks resolved as far as it exists, so two
// spellings of one directory compare equal whether or not it exists yet.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || filepath.Clean(dir) == p {
		return p
	}
	return filepath.Join(resolvePath(dir), base)
}

// readClientID reads a valid client id at path; "" when there is none or it
// is malformed. it takes the lock beside it, shared, so it never sees a
// half-written id; it writes nothing.
func readClientID(path string) string {
	if lock, err := os.Open(path + ".lock"); err == nil {
		defer lock.Close()
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_SH) == nil {
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(b)); clientIDRe.MatchString(id) {
		return id
	}
	return ""
}

// knownOwner is this host's owner label as it stands, read only: the client
// id in the cache dir, else the legacy one, else none (""). clean judges by
// it, so a clean, dry run or not, never makes or repairs an id.
func knownOwner() string {
	id := ""
	if dir, err := CacheDir(); err == nil {
		id = readClientID(filepath.Join(dir, "client-id"))
	}
	if legacy := legacyCacheDir(); id == "" && legacy != "" {
		id = readClientID(filepath.Join(legacy, "client-id"))
	}
	if id == "" {
		return ""
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getuid(), id)
}
