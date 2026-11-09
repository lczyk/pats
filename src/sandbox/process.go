package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// how the drivers are put together, every one the same way: a driver is
// built from a process (what one pats process holds, its clock and its
// waits), a runner for its cli (lxc, docker, podman) and Options (which the
// caller fills from the environment, the driver never). every cli call goes
// through the runner, every wait through the clock. so a test hands a driver
// a fake runner and a process of its own, and runs the production code.

// process is what one pats process holds across its sandboxes: its run
// token and owner stamp, the clock and waits the drivers go by, the egress
// subnet counter, and what a driver sets up once per server. production uses
// defaultProcess; a test makes its own, so two of them on one fake server are
// two pats runs.
type process struct {
	token string
	owner func() string // read on first use: it may persist the client id
	clock clock
	t     timings
	reg   registry
	// home is the per-user dir the temp root and the lease files live in.
	home func() (string, error)
	// legacy is an older pats's home, read by clean; "" for none.
	legacy func() string
	// knownOwner is the owner label as it stands, read only (clean judges by
	// it); "" when this host has no client id yet.
	knownOwner func() string
	lease      hostLease

	slot atomic.Uint32 // the next egress subnet

	lxd lxdProcess
}

// lxdProcess is the lxd driver's per-process state: each server's setup and
// lease renewal, and its proxy image.
type lxdProcess struct {
	mu      sync.Mutex
	servers map[string]*lxdServer

	proxyMu     sync.Mutex
	proxyImages map[string]string
}

// timings are the waits and bounds the drivers use. the waits between tries
// go by the clock; the bounds put on a context (instanceGone, cliCleanup,
// proxyFetch) are wall time, since a context deadline is.
type timings struct {
	leaseEvery    time.Duration // how often a live run renews its lease
	leaseWake     time.Duration // how often the renewal looks at the clock
	leaseStale    time.Duration // how old a lease is before clean takes the run as gone
	instanceGone  time.Duration // how long a delete waits out a server operation on the instance
	deleteRetry   time.Duration
	ready         time.Duration // an instance's exec and route coming up
	readyPoll     time.Duration
	onlineRetry   time.Duration
	proxyReady    time.Duration // the egress proxy's listening line
	execGrace     time.Duration // a cancelled exec's sigterm to sigkill
	followerGrace time.Duration // a log follower's own exit, once what it follows is gone
	cliCleanup    time.Duration // removing a failed run's container, after the caller's context is gone
	proxyFetch    time.Duration // the egress proxy binary from the registry
}

var defaultTimings = timings{
	leaseEvery:    2 * time.Minute,
	leaseWake:     30 * time.Second,
	leaseStale:    10 * time.Minute,
	instanceGone:  5 * time.Minute,
	deleteRetry:   2 * time.Second,
	ready:         3 * time.Minute,
	readyPoll:     time.Second,
	onlineRetry:   2 * time.Second,
	proxyReady:    readyWait,
	execGrace:     10 * time.Second,
	followerGrace: 2 * time.Second,
	cliCleanup:    30 * time.Second,
	proxyFetch:    5 * time.Minute,
}

func newProcess(c clock, t timings) *process {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &process{token: hex.EncodeToString(b), owner: sync.OnceValue(hostOwner), clock: c, t: t, reg: defaultRegistry, home: cacheDir,
		legacy: legacyCacheDir, knownOwner: knownOwner,
		lxd: lxdProcess{servers: map[string]*lxdServer{}, proxyImages: map[string]string{}}}
}

var defaultProcess = newProcess(wallClock{}, defaultTimings)

// nextSubnet hands out the next /29 of pool. it only orders attempts within
// this process: another pats on the same daemon, or a network a crashed run
// left behind, makes `network create` fail, and the caller moves on.
func (p *process) nextSubnet(pool netip.Prefix) netip.Prefix {
	n := p.slot.Add(1) - 1
	count := uint32(1) << (slotBits - pool.Bits())
	base := pool.Addr().As4()
	off := (n % count) << (32 - slotBits)
	ip := netip.AddrFrom4([4]byte{
		base[0] | byte(off>>24), base[1] | byte(off>>16), base[2] | byte(off>>8), base[3] | byte(off),
	})
	return netip.PrefixFrom(ip, slotBits)
}

type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) ticker
}

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (wallClock) NewTicker(d time.Duration) ticker       { return wallTicker{time.NewTicker(d)} }

type wallTicker struct{ t *time.Ticker }

func (w wallTicker) C() <-chan time.Time { return w.t.C }
func (w wallTicker) Stop()               { w.t.Stop() }

// runner executes a driver's cli: Run to completion, or Start for a process
// the caller signals and waits on (an agent's exec, a follower).
type runner interface {
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
	Start(stdin io.Reader, stdout, stderr io.Writer, args ...string) (proc, error)
}

type proc interface {
	Wait() error
	Signal(os.Signal) error
}

// execRunner runs bin, with env on top of this process's. waitDelay bounds
// how long Wait copies output after the process is gone, should something
// it left hold the pipes open.
type execRunner struct {
	bin       string
	env       []string
	waitDelay time.Duration
}

// cliRunner runs bin.
func cliRunner(bin string) runner { return execRunner{bin: bin, waitDelay: 10 * time.Second} }

var lxcCLI = cliRunner("lxc")

// environ is the cli's environment: nil (this process's) when r adds none.
func (r execRunner) environ() []string {
	if len(r.env) == 0 {
		return nil
	}
	return append(os.Environ(), r.env...)
}

func (r execRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = r.environ()
	return cmd.Run()
}

func (r execRunner) Start(stdin io.Reader, stdout, stderr io.Writer, args ...string) (proc, error) {
	cmd := exec.Command(r.bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = r.environ()
	cmd.WaitDelay = r.waitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProc{cmd}, nil
}

type execProc struct{ cmd *exec.Cmd }

func (p execProc) Wait() error              { return p.cmd.Wait() }
func (p execProc) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }

// combined runs args to completion and returns their output, stdout and
// stderr together, trimmed.
func combined(ctx context.Context, r runner, args ...string) (string, error) {
	var out bytes.Buffer
	err := r.Run(ctx, nil, &out, &out, args...)
	return strings.TrimSpace(out.String()), err
}

// output runs args to completion and returns their stdout alone, for reading
// data; stderr goes in the error, so a failure says more than its status.
func output(ctx context.Context, r runner, args ...string) ([]byte, error) {
	var out, errb bytes.Buffer
	err := r.Run(ctx, nil, &out, &errb, args...)
	if err != nil && errb.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), err
}

// execCancellable runs args to completion. when ctx ends first, cancel runs
// (whatever stops the work the client started, which ending the client
// alone does not), then the client gets sigterm, and sigkill after the
// grace. a process that finished as ctx ended keeps its result. after a
// cancel an exit status wins and a clean exit reports ctx's error, as
// os/exec's Wait does.
func (p *process) execCancellable(ctx context.Context, r runner, stdin io.Reader, stdout, stderr io.Writer, cancel func(), args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	x, err := r.Start(stdin, stdout, stderr, args...)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- x.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	select {
	case err := <-done:
		return err
	default:
	}
	cancel()
	_ = x.Signal(syscall.SIGTERM)
	select {
	case err = <-done:
	case <-p.clock.After(p.t.execGrace):
		_ = x.Signal(syscall.SIGKILL)
		err = <-done
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// exitCoder is what an exit status error has: *exec.ExitError, or a fake's.
type exitCoder interface{ ExitCode() int }

// asExitError finds an exit status in err. os/exec can wrap it (a cancel
// racing the exit), so it is looked for with errors.As.
func asExitError(err error) (exitCoder, bool) {
	var e exitCoder
	ok := errors.As(err, &e)
	return e, ok
}

// exitResult reads a cli exec's error: an exit status is the command's
// result, anything else is the exec failing, named by what.
func exitResult(err error, what string) (int, error) {
	if err == nil {
		return 0, nil
	}
	if e, ok := asExitError(err); ok {
		return e.ExitCode(), nil
	}
	return -1, fmt.Errorf("%s: %w", what, err)
}

// follower is a long-running process whose stdout goes to the caller and
// whose stderr carries a listening line: the lxd egress proxy, docker's
// follower of the proxy's logs.
type follower struct {
	x      proc
	done   chan struct{} // closed once x has exited
	stderr *io.PipeReader
}

func follow(r runner, stdin io.Reader, stdout io.Writer, args ...string) (*follower, error) {
	pr, pw := io.Pipe()
	x, err := r.Start(stdin, stdout, pw, args...)
	if err != nil {
		pw.Close()
		return nil, err
	}
	f := &follower{x: x, done: make(chan struct{}), stderr: pr}
	go func() {
		_ = x.Wait()
		pw.Close()
		close(f.done)
	}()
	return f, nil
}

// ready waits for the listening line, up to timeout.
func (f *follower) ready(c clock, timeout time.Duration) error {
	return waitReady(f.stderr, c, timeout)
}

// stop gives the follower grace to exit on its own, then kills it, and
// waits.
func (f *follower) stop(c clock, grace time.Duration) {
	select {
	case <-f.done:
		return
	case <-c.After(grace):
	}
	_ = f.x.Signal(os.Kill)
	<-f.done
}
