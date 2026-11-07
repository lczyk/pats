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

// manualClock is a clock that moves only when a test advances it: After and
// a ticker fire as it passes their time. an After of zero or less fires at
// once.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []manualWaiter
	tickers []*manualTicker
}

type manualWaiter struct {
	at time.Time
	ch chan time.Time
}

type manualTicker struct {
	c       *manualClock
	every   time.Duration
	next    time.Time
	ch      chan time.Time
	stopped bool
}

func newManualClock(now time.Time) *manualClock { return &manualClock{now: now} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, manualWaiter{c.now.Add(d), ch})
	return ch
}

func (c *manualClock) NewTicker(d time.Duration) ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTicker{c: c, every: d, next: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	return t
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }

func (t *manualTicker) Stop() {
	t.c.mu.Lock()
	t.stopped = true
	t.c.mu.Unlock()
}

// Advance moves the clock on by d, firing what falls due. a ticker drops
// ticks its reader has not taken, as time.Ticker does.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var left []manualWaiter
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			left = append(left, w)
		}
	}
	c.waiters = left
	for _, t := range c.tickers {
		for !t.stopped && !t.next.After(c.now) {
			select {
			case t.ch <- t.next:
			default:
			}
			t.next = t.next.Add(t.every)
		}
	}
}
