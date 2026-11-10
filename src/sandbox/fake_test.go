package sandbox

import (
	"context"
	"fmt"
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
		leaseEvery: time.Hour, leaseWake: time.Hour, leaseStale: 10 * time.Minute, instanceGone: 5 * time.Second,
		deleteRetry: time.Millisecond, ready: 5 * time.Second, readyPoll: time.Millisecond,
		onlineRetry: time.Millisecond, proxyReady: 5 * time.Second, execGrace: 100 * time.Millisecond,
		followerGrace: 100 * time.Millisecond, cliCleanup: 5 * time.Second, proxyFetch: 5 * time.Second,
		stamp: 30 * time.Second, childReady: 5 * time.Second,
	})
	home := t.TempDir()
	p.home = func() (string, error) { return home, nil }
	p.legacy = func() string { return "" }
	// an owner of the test binary's own: a test clean on a real daemon then
	// judges a real pats run there (labelled with this host's owner) as
	// another's, and leaves it.
	p.owner = func() string { return testOwner }
	p.knownOwner = func() string { return p.owner() }
	t.Cleanup(p.shutdown)
	return p
}

// testOwner is the owner label every test process uses.
var testOwner = fmt.Sprintf("pats-test/%d/%d", os.Getuid(), os.Getpid())

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
			w.ch <- w.at
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

// fakeFault fails one call of a fake cli: the fake does nothing of it, or,
// with after, does it and the client still sees a failure. with cancel, the
// call's context is ended there instead (a deadline, ctrl+c): before the
// fake acts, or after, when the client is killed while the daemon carries
// on.
type fakeFault struct {
	at     int // the call it hits, counting from 0
	after  bool
	cancel context.CancelFunc
}

// faults puts at most one fault into a fake cli's calls.
type faults struct {
	mu      sync.Mutex
	plan    *fakeFault
	n       int    // calls so far
	hit     bool   // the fault was reached
	hitCall string // the call it hit
}

// arm faults the at-th call from now.
func (fs *faults) arm(at int, after bool, cancel context.CancelFunc) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.plan = &fakeFault{at: fs.n + at, after: after, cancel: cancel}
}

// off ends fault injection: what follows is outside the swept calls.
func (fs *faults) off() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.plan != nil && fs.plan.at >= fs.n {
		fs.plan = nil
	}
}

func (fs *faults) reached() (bool, string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hit, fs.hitCall
}

// call serves one call of a fake cli through do, faulting it if it is the
// planned one. do is told when the client dies after the fake acted.
func (fs *faults) call(ctx context.Context, args []string, stderr io.Writer, do func(killed bool) error) error {
	fs.mu.Lock()
	n := fs.n
	fs.n++
	var f *fakeFault
	if fs.plan != nil && fs.plan.at == n {
		f = fs.plan
		fs.hit, fs.hitCall = true, strings.Join(args, " ")
	}
	fs.mu.Unlock()
	// a context already done starts nothing, as with os/exec.
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case f == nil:
		return do(false)
	case !f.after && f.cancel != nil:
		f.cancel()
		return exitStatus(-1)
	case !f.after:
		fmt.Fprintln(stderr, "Error: injected fault")
		return exitStatus(1)
	case f.cancel != nil:
		_ = do(true)
		f.cancel()
		return exitStatus(-1)
	}
	_ = do(false)
	fmt.Fprintln(stderr, "Error: injected fault after the daemon acted")
	return exitStatus(1)
}

// faultKinds are the ways one cli call goes wrong: the server refuses it, or
// does it and the client still fails; or the run's context ends there, before
// the server acts or after (an init then finishing on the server).
var faultKinds = []struct {
	name          string
	after, cancel bool
}{
	{"fail", false, false},
	{"fail-after", true, false},
	{"cancel", false, true},
	{"cancel-after", true, true},
}

// isRemoval says whether a cli call removes something: lxc's deletes,
// docker's rm, network rm and kill.
func isRemoval(call string) bool {
	for _, p := range []string{"delete ", "rm ", "network rm ", "kill "} {
		if strings.HasPrefix(call, p) {
			return true
		}
	}
	return strings.Contains(call, " delete ")
}
