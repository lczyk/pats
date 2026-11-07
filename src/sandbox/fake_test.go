package sandbox

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// the fakes every driver's tests share: a runner from a function, the procs
// it starts, the calls a fake cli saw, and a process with short waits. see
// docs/spec/sandbox-seams.md.

// runFunc is a runner from a function: Start runs it in a goroutine whose
// context a signal ends.
type runFunc func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error

func (f runFunc) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return f(ctx, stdin, stdout, stderr, args...)
}

func (f runFunc) Start(stdin io.Reader, stdout, stderr io.Writer, args ...string) (proc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakeProc{cancel: cancel, done: make(chan struct{})}
	go func() {
		p.err = f(ctx, stdin, stdout, stderr, args...)
		close(p.done)
	}()
	return p, nil
}

type fakeProc struct {
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	ignore  os.Signal // a signal that does not end it
	mu      sync.Mutex
	signals []os.Signal
}

func (p *fakeProc) Wait() error { <-p.done; return p.err }

func (p *fakeProc) Signal(s os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, s)
	p.mu.Unlock()
	if s != p.ignore {
		p.cancel()
	}
	return nil
}

func (p *fakeProc) got() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

// procRunner is runFunc keeping the procs it starts, which ignore ignore.
type procRunner struct {
	runFunc
	ignore os.Signal
	mu     sync.Mutex
	procs  []*fakeProc
}

func (r *procRunner) Start(stdin io.Reader, stdout, stderr io.Writer, args ...string) (proc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakeProc{cancel: cancel, done: make(chan struct{}), ignore: r.ignore}
	r.mu.Lock()
	r.procs = append(r.procs, p)
	r.mu.Unlock()
	go func() {
		p.err = r.runFunc(ctx, stdin, stdout, stderr, args...)
		close(p.done)
	}()
	return p, nil
}

// testProcess is a process with waits short enough for unit tests, shut
// down when the test ends.
func testProcess(t *testing.T) *process {
	p := newProcess(wallClock{}, timings{
		leaseEvery: time.Hour, leaseStale: 10 * time.Minute, instanceGone: 5 * time.Second,
		deleteRetry: time.Millisecond, ready: 5 * time.Second, readyPoll: time.Millisecond,
		onlineRetry: time.Millisecond, proxyReady: 5 * time.Second, execGrace: 100 * time.Millisecond,
		followerGrace: 100 * time.Millisecond, cliCleanup: 5 * time.Second, proxyFetch: 5 * time.Second,
	})
	t.Cleanup(p.shutdown)
	return p
}

// fixedClock is the wall clock but for Now.
type fixedClock struct {
	wallClock
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

// exitStatus is a fake's exit status error, as *exec.ExitError is a real
// one's: -1 for a client a signal ended.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// callLog is the calls a fake cli saw, safe across the goroutines a Start
// runs in.
type callLog struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *callLog) record(args []string) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
}

// all is every call so far, one string each.
func (f *callLog) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func (f *callLog) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// call is the first call starting with prefix.
func (f *callLog) call(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return c
		}
	}
	return nil
}

// find is the first call containing sub.
func (f *callLog) find(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return c
		}
	}
	return nil
}
