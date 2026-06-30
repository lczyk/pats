package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockConfigDir takes an exclusive, non-blocking flock on .pats/lock so two
// mutating commands (run, score) can't operate on the same config dir at once.
// the kernel drops the lock on process exit, so a crashed run leaves no stale
// lockfile. returns an unlock func to defer.
//
// NOTE: unix-only (flock). add a windows build-tagged variant if windows ever
// needs to run pats.
func lockConfigDir(configDir string) (func(), error) {
	if err := os.MkdirAll(filepath.Join(configDir, ".pats"), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(configDir, ".pats", "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another pats command is active in %s (.pats/lock held): %w", configDir, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
