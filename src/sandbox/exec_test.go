package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// execBackend is one driver as the Exec cancel test drives it: a session on
// a fake cli, and the call its cancel step makes for an agent and a root
// step. kills is a driver whose client is the sandbox: it has no cancel step
// and no grace, the client is killed outright.
type execBackend struct {
	name        string
	session     func(t *testing.T, p *process, r runner) Session
	agent, root string
	kills       bool
}

var execBackends = []execBackend{
	{
		name: "lxd",
		session: func(_ *testing.T, p *process, r runner) Session {
			return &lxdSession{l: newLXD(p, r, Options{Remote: "srv"}), name: "pats-x", uid: "1001"}
		},
		agent: "pkill -KILL -u 1001",
		root:  "stop srv:pats-x --project pats --force",
	},
	{
		name: "docker",
		session: func(_ *testing.T, p *process, r runner) Session {
			return &containerSession{c: newContainer(p, r, Options{Driver: "docker"}).on(SandboxOptions{Image: "img"}), id: "cid", teardown: func() {}}
		},
		agent: "kill cid",
		root:  "kill cid",
	},
	{
		name: "bwrap",
		session: func(t *testing.T, p *process, r runner) Session {
			return &bwrapSession{d: &bwrapDriver{p: p, r: r}, rootfs: "/store/rootfs/sha256-x", dir: t.TempDir(), workdir: "/work"}
		},
		kills: true,
	},
}

// every driver's Exec, cancelled: the cancel step stops the work inside (the
// cli client ending alone leaves it running), then the client gets sigterm,
// and sigkill after the grace if it outlives that. the killed client's exit
// status is the result, not an error, as with a real one; one that exits 0
// once signalled reports the cancel. one that finishes on its own is not
// signalled and nothing is cancelled, and on a context already done nothing
// starts: ErrLaunch. host, with no client and no runner to fake, is held to
// the same results on real processes.
func TestExecCancel(t *testing.T) {
	for _, b := range execBackends {
		for _, tc := range []struct {
			name    string
			root    bool
			ignore  os.Signal
			clean   bool // the client exits 0 once signalled
			signals []os.Signal
		}{
			{"agent", false, nil, false, []os.Signal{syscall.SIGTERM}},
			{"root", true, nil, false, []os.Signal{syscall.SIGTERM}},
			{"stubborn", false, syscall.SIGTERM, false, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}},
			{"clean exit", false, nil, true, []os.Signal{syscall.SIGTERM}},
		} {
			t.Run(b.name+"/"+tc.name, func(t *testing.T) {
				var calls callLog
				r := &procRunner{ignore: tc.ignore, runFunc: func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
					calls.record(args)
					if args[len(args)-1] == "agent" {
						<-ctx.Done() // a signal ends the fake client
						if tc.clean {
							return nil
						}
						return exitStatus(-1)
					}
					return nil
				}}
				s := b.session(t, testProcess(t), r)
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					for calls.find("agent") == nil {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
				code, err := execBounded(t, s, ctx, Cmd{Argv: []string{"agent"}, Root: tc.root})
				if tc.clean {
					require.Error(t, err, "context canceled")
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, code, -1)
				want, signals := b.agent, tc.signals
				if tc.root {
					want = b.root
				}
				if b.kills {
					signals = []os.Signal{syscall.SIGKILL}
					assert.Equal(t, len(calls.all()), 1, "a cancel step ran: %v", calls.all())
				} else {
					assert.ContainsString(t, strings.Join(calls.all(), "\n"), want)
				}
				require.Equal(t, len(r.procs), 1)
				assert.Equal(t, fmt.Sprint(r.procs[0].got()), fmt.Sprint(signals))
			})
		}

		t.Run(b.name+"/done before", func(t *testing.T) {
			r := &procRunner{runFunc: func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error { return nil }}
			s := b.session(t, testProcess(t), r)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			code, err := execBounded(t, s, ctx, Cmd{Argv: []string{"agent"}})
			require.Error(t, err, "context canceled")
			assert.ErrorIs(t, err, ErrLaunch)
			assert.Equal(t, code, -1)
			assert.Equal(t, len(r.procs), 0)
		})

		t.Run(b.name+"/finishes", func(t *testing.T) {
			var calls callLog
			r := &procRunner{runFunc: func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
				calls.record(args)
				return nil
			}}
			s := b.session(t, testProcess(t), r)
			code, err := execBounded(t, s, context.Background(), Cmd{Argv: []string{"true"}})
			require.NoError(t, err)
			assert.Equal(t, code, 0)
			assert.Equal(t, len(r.procs[0].got()), 0)
			assert.Equal(t, len(calls.all()), 1)
		})
	}

	host := func(t *testing.T) (Session, string) {
		dir := t.TempDir()
		s, err := hostOn(t, Options{}).Start(context.Background(), Spec{Workdir: dir, Egress: Egress{None: true}})
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })
		return s, dir
	}
	t.Run("host/agent", func(t *testing.T) {
		s, _ := host(t)
		// the command's stdout is a pipe of the test's: it ends once nothing
		// of the command's group holds it, a child in the background (as an
		// agent's tools are) included, alive or not yet reaped.
		pr, pw, err := os.Pipe()
		require.NoError(t, err)
		defer pr.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := s.Exec(ctx, Cmd{Argv: []string{"/bin/sh", "-c", "sleep 300 & echo started; wait"}}, pw, io.Discard)
			pw.Close()
			done <- result{code, err}
		}()
		require.NoError(t, pr.SetReadDeadline(time.Now().Add(10*time.Second)))
		out := bufio.NewReader(pr)
		line, err := out.ReadString('\n')
		require.NoError(t, err, "the command never started")
		require.Equal(t, line, "started\n")
		cancel()
		select {
		case got := <-done:
			require.NoError(t, got.err)
			assert.Equal(t, got.code, -1)
		case <-time.After(5 * time.Second):
			t.Fatal("Exec did not return")
		}
		require.NoError(t, pr.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = io.ReadAll(out)
		assert.NoError(t, err, "the cancelled command's child still holds its stdout")
	})
	t.Run("host/done before", func(t *testing.T) {
		s, dir := host(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		code, err := execBounded(t, s, ctx, Cmd{Argv: []string{"/bin/sh", "-c", "touch ran"}})
		require.Error(t, err, "context canceled")
		assert.ErrorIs(t, err, ErrLaunch)
		assert.Equal(t, code, -1)
		_, err = os.Stat(filepath.Join(dir, "ran"))
		assert.That(t, os.IsNotExist(err), "the command ran")
	})
	t.Run("host/finishes", func(t *testing.T) {
		s, _ := host(t)
		code, err := execBounded(t, s, context.Background(), Cmd{Argv: []string{"true"}})
		require.NoError(t, err)
		assert.Equal(t, code, 0)
	})
}

// execBounded runs s.Exec and fails the test, rather than hanging it, when
// Exec does not return: a lost sigkill would otherwise wait forever.
func execBounded(t *testing.T, s Session, ctx context.Context, cmd Cmd) (int, error) {
	t.Helper()
	type res struct {
		code int
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		code, err := s.Exec(ctx, cmd, io.Discard, io.Discard)
		ch <- res{code, err}
	}()
	select {
	case r := <-ch:
		return r.code, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return")
		return 0, nil
	}
}
