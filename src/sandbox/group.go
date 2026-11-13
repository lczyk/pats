package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// RunGroup runs cmd in its own process group, and a step's processes end
// with it: the group is killed when ctx ends while it runs, and when the
// script exits, so nothing it started in the background outlives it. the
// output goes through pipes runGroup owns, drained only after that kill: a
// child left holding the output cannot keep the step waiting.
func RunGroup(ctx context.Context, cmd *exec.Cmd) error {
	var ours []*os.File // the child's ends, closed in this process once it has them
	var reads []*os.File
	var drains []chan struct{}
	var copyMu sync.Mutex
	var copyErr error // the first output that could not be written
	piped := map[io.Writer]*os.File{}
	wire := func(dst io.Writer) (io.Writer, error) {
		if _, isFile := dst.(*os.File); isFile || dst == nil {
			return dst, nil
		}
		if w, ok := piped[dst]; ok { // stdout and stderr to one writer: one pipe
			return w, nil
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := io.Copy(dst, r); err != nil {
				// closed, the pipe fails the child's next write rather than
				// filling up and stalling it.
				r.Close()
				copyMu.Lock()
				if copyErr == nil {
					copyErr = fmt.Errorf("output: %w", err)
				}
				copyMu.Unlock()
			}
		}()
		piped[dst], ours, reads, drains = w, append(ours, w), append(reads, r), append(drains, done)
		return w, nil
	}
	// finish closes every pipe and joins every drain. with waitDrains it
	// first lets them end on their own: the group is gone, so the pipes'
	// other ends are closed. once the delay passes, those still open (a
	// child that left the group holds them) are cut off, and it says so.
	finish := func(waitDrains bool) (cut bool) {
		for _, w := range ours {
			w.Close()
		}
		if waitDrains {
			timeout := time.After(GroupWaitDelay)
		wait:
			for _, d := range drains {
				select {
				case <-d:
				case <-timeout:
					cut = true
					break wait
				}
			}
		}
		for _, r := range reads {
			r.Close()
		}
		for _, d := range drains {
			<-d
		}
		return cut
	}
	var err error
	if cmd.Stdout, err = wire(cmd.Stdout); err == nil {
		cmd.Stderr, err = wire(cmd.Stderr)
	}
	// a context done before the start is no start at all: the caller tells
	// it by cmd.Process being nil.
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		finish(false)
		return err
	}
	for _, w := range ours {
		w.Close()
	}
	ours = nil
	pid := cmd.Process.Pid
	kill := func() { syscall.Kill(-pid, syscall.SIGKILL) }
	stop := context.AfterFunc(ctx, kill)
	defer stop()
	err = cmd.Wait()
	kill()
	cut := finish(true)
	copyMu.Lock()
	defer copyMu.Unlock()
	switch {
	case err != nil:
	case cut:
		err = errors.New("output cut off: a process that left the step's process group still held it")
	default:
		err = copyErr
	}
	return err
}

// GroupWaitDelay bounds the wait for a finished group's output to drain,
// should something outside it still hold the pipe.
var GroupWaitDelay = 10 * time.Second
