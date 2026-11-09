package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// a run's lease on this host: run-<token>.lease in the pats cache dir, beside
// the temp root, made before the run's first container or temp dir. it names
// the host and pid that wrote it, and its modification time is renewed while
// the run lives, so `pats clean` tells a live run from a dead one by it.
// lxd keeps its runs' leases on the server instead (the marker acl).

// leaseSuffix and leasePrefix spell a lease file's name.
const (
	leasePrefix = "run-"
	leaseSuffix = ".lease"
)

// hostLease is this process's lease file, made once.
type hostLease struct {
	once       sync.Once
	err        error
	path       string
	stop, done chan struct{}
}

// cacheDir is where pats keeps its per-user state: the temp root, the
// client id, the lease files.
func cacheDir() (string, error) {
	return CacheDir()
}

// leasePath is the lease file of a run.
func (p *process) leasePath(token string) (string, error) {
	dir, err := p.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, leasePrefix+token+leaseSuffix), nil
}

// holdLease makes this run's lease file, once, and keeps it fresh until
// shutdown.
func (p *process) holdLease() error {
	x := &p.lease
	x.once.Do(func() {
		path, err := p.leasePath(p.token)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(path), 0o755)
		}
		if err == nil {
			err = p.writeLease(path)
		}
		if err != nil {
			x.err = fmt.Errorf("run lease: %w", err)
			return
		}
		x.path, x.stop, x.done = path, make(chan struct{}), make(chan struct{})
		go p.renewLease(p.clock.NewTicker(p.t.leaseWake), p.clock.Now())
	})
	return x.err
}

// writeLease writes the lease's host and pid, its time the clock's now.
func (p *process) writeLease(path string) error {
	host, _ := os.Hostname()
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s %d\n", host, os.Getpid()), 0o644); err != nil {
		return err
	}
	now := p.clock.Now()
	return os.Chtimes(path, now, now)
}

// renewLease keeps the lease file fresh, last renewed at last. it wakes at
// t (every leaseWake) and renews once leaseEvery has passed on the wall
// clock, so a machine waking from sleep renews within a wake, not a whole
// period later.
func (p *process) renewLease(t ticker, last time.Time) {
	x := &p.lease
	defer close(x.done)
	defer t.Stop()
	for {
		select {
		case <-x.stop:
			return
		case <-t.C():
			now := p.clock.Now()
			if now.Round(0).Sub(last.Round(0)) < p.t.leaseEvery {
				continue
			}
			if err := os.Chtimes(x.path, now, now); errors.Is(err, fs.ErrNotExist) {
				_ = p.writeLease(x.path) // a clean took it: the run is alive
			}
			last = now
		}
	}
}

// endLease stops the renewal and removes the lease file.
func (p *process) endLease() {
	x := &p.lease
	if x.stop == nil {
		return
	}
	close(x.stop)
	<-x.done
	_ = os.Remove(x.path)
	x.stop = nil
}

// leaseFile is a run's lease as its file says.
type leaseFile struct {
	host string
	pid  int
	at   time.Time
}

// leaseFound is where a run's lease file is on this host: the cache dir's,
// else the legacy dir's (an older pats's run), else the cache dir's path.
func (p *process) leaseFound(token string) (string, error) {
	path, err := p.leasePath(token)
	if err != nil {
		return "", err
	}
	// a lease that cannot be looked at is not an absent one: its run is
	// judged unknown, never dead by its age.
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if legacy := p.legacy(); legacy != "" {
		old := filepath.Join(legacy, leasePrefix+token+leaseSuffix)
		if _, err := os.Stat(old); err == nil {
			return old, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return path, nil
}

// readLease reads a run's lease file on this host, in the cache dir or the
// legacy one; ok is false when there is none.
func (p *process) readLease(token string) (l leaseFile, ok bool, err error) {
	path, err := p.leaseFound(token)
	if err != nil {
		return l, false, err
	}
	return readLeaseAt(path)
}

// readLeaseAt reads the lease file at path; ok is false when there is none.
func readLeaseAt(path string) (l leaseFile, ok bool, err error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, false, nil
	} else if err != nil {
		return l, false, err
	}
	l.at = st.ModTime()
	b, err := os.ReadFile(path)
	if err != nil {
		return l, false, err
	}
	host, pid, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	l.host = host
	l.pid, _ = strconv.Atoi(pid)
	return l, true, nil
}
