package sandbox

import (
	"os"
	"path/filepath"
)

// MkTemp creates a temp dir suitable for bind-mounting into a sandbox. it is
// NOT the system /tmp: a confined docker daemon (notably the docker snap) runs
// in its own mount namespace and can't see the host /tmp, so a `-v /tmp/...`
// silently mounts an empty dir instead of the files we staged. os.UserCacheDir()
// lives under $HOME, which such daemons do share. the caller removes the dir.
//
// NOTE: if XDG_CACHE_HOME points outside $HOME this breaks the same way -- the
// fix then is to unset it. a fully remote daemon (no shared fs at all) can't be
// helped by any host path; that's out of scope.
func MkTemp(prefix string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(cache, "pats", "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, prefix)
}
