package sandbox

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// flockRetry is the pause between tries of a lock another process holds.
const flockRetry = 50 * time.Millisecond

// Flock takes the advisory lock on f, exclusive or shared, waiting for
// whoever holds it until ctx ends: flock(2) itself waits for ever, through
// a ctrl-c the caller has caught. so it is tried, not waited on.
func Flock(ctx context.Context, f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(flockRetry):
		}
	}
}
