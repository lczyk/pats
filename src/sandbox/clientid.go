package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// clientID is a random id made once per user and kept beside the temp root:
// host and uid alone collide across machines sharing a daemon, and clean
// would then judge another machine's live run by an owner file it cannot
// see.
var clientID = sync.OnceValue(func() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	fresh := hex.EncodeToString(b)
	dir, err := CacheDir()
	if err != nil {
		// unpersisted: unique to this process, so never another's; a later
		// clean sees what it made as foreign and leaves it to --all.
		return fresh
	}
	// a pats before CacheDir kept its id in the legacy dir: carried over, so
	// runs an older binary started stay this client's.
	if id, err := loadClientID(filepath.Join(dir, "client-id"), func() string {
		if legacy := legacyCacheDir(); legacy != "" {
			if id := readClientID(filepath.Join(legacy, "client-id")); id != "" {
				return id
			}
		}
		return fresh
	}); err == nil {
		return id
	}
	return fresh
})

// loadClientID returns the id stored at p, publishing fresh() there first
// when there is none (fresh runs under p's lock, so a legacy id it reads is
// read under both locks, p's first): written to a temp file and hard-linked into place, so the
// file is whole whenever it exists. a malformed file (from before this) is
// replaced. a lock beside it serialises the whole read-repair-publish, so
// concurrent first runs, and concurrent repairs, all end on one id.
func loadClientID(p string, fresh func() string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	read := func() (string, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		if id := strings.TrimSpace(string(b)); clientIDRe.MatchString(id) {
			return id, nil
		}
		return "", errMalformedID
	}
	id, err := read()
	if err == nil {
		return id, nil
	}
	if errors.Is(err, errMalformedID) {
		if err := os.Remove(p); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "client-id-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(fresh() + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), p); err != nil && !os.IsExist(err) {
		return "", err
	}
	return read()
}

var errMalformedID = errors.New("malformed client id")

var clientIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
