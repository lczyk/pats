//go:build !nobwrap

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// bwrapCall is one bwrap a fake runner saw: its argv and what came on stdin.
type bwrapCall struct {
	args  []string
	stdin string
}

type fakeBwrap struct {
	calls []bwrapCall
	code  int
}

func (f *fakeBwrap) Run(context.Context, io.Reader, io.Writer, io.Writer, ...string) error {
	return nil
}
func (f *fakeBwrap) Start(stdin io.Reader, _, _ io.Writer, args ...string) (proc, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.calls = append(f.calls, bwrapCall{args: args, stdin: string(in)})
	return doneProc{code: f.code}, nil
}

type doneProc struct{ code int }

func (p doneProc) Wait() error {
	if p.code != 0 {
		return exitStatus(p.code)
	}
	return nil
}
func (p doneProc) Signal(os.Signal) error { return nil }

func bwrapSessionOn(f *fakeBwrap, net netMode) *bwrapSession {
	d := &bwrapDriver{r: f}
	s := &bwrapSession{d: d, rootfs: "/store/rootfs/sha256-x", dir: "/cache/pats-bwrap-1", workdir: "/work",
		imageEnv: []string{"PATH=/opt/tools/bin:/usr/bin", "LANG=C.UTF-8"}, net: net,
		binds: []bind{{host: "/home-tmp", path: HomeMount}, {host: "/creds", path: "/creds", ro: true}}}
	if net == netProxy {
		s.proxy = &proxyChild{forwarder: "/store/pats-static"}
		s.env = map[string]string{"HTTPS_PROXY": "http://127.0.0.1:8080"}
	}
	return s
}

func argString(args []string) string { return strings.Join(args, " ") }

func TestBwrapArgv(t *testing.T) {
	for _, mode := range []struct {
		name string
		net  netMode
	}{{"open", netOpen}, {"none", netNone}, {"proxy", netProxy}} {
		t.Run(mode.name, func(t *testing.T) {
			f := &fakeBwrap{}
			s := bwrapSessionOn(f, mode.net)
			private := map[string]string{"PATS_TEST_VALUE": "never-on-argv"}
			_, err := s.Exec(context.Background(), Cmd{Argv: []string{"agent", "--go"}, Env: private}, io.Discard, io.Discard)
			require.NoError(t, err)
			_, err = s.Exec(context.Background(), Cmd{Argv: []string{"prepare"}, Root: true}, io.Discard, io.Discard)
			require.NoError(t, err)
			require.Equal(t, len(f.calls), 2)
			agent, root := f.calls[0], f.calls[1]
			a := argString(agent.args)
			assert.ContainsString(t, a, "--overlay-src /store/rootfs/sha256-x --overlay /cache/pats-bwrap-1/upper /cache/pats-bwrap-1/work /")
			assert.ContainsString(t, a, "--bind /work /workspace")
			assert.ContainsString(t, a, "--bind /home-tmp /pats-home")
			assert.ContainsString(t, a, "--ro-bind /creds /creds")
			assert.ContainsString(t, a, "--uid "+strconv.Itoa(os.Getuid())+" --gid "+strconv.Itoa(os.Getgid()))
			assert.ContainsString(t, a, "--clearenv --chdir /workspace --")
			assert.That(t, !strings.Contains(a, "--cap-add"), "the agent got capabilities: %s", a)
			assert.That(t, !strings.Contains(a, "never-on-argv"), "an env value on bwrap's argv: %s", a)
			assert.ContainsString(t, agent.stdin, "export PATS_TEST_VALUE='never-on-argv'")
			assert.ContainsString(t, agent.stdin, "export PATH='/opt/tools/bin:/usr/bin'") // the image's Env holds
			assert.Equal(t, strings.Contains(a, "--unshare-net"), mode.net != netOpen)
			fwd := "/run/pats/pats egress forward --listen 127.0.0.1:8080 --socket /run/pats-egress/proxy.sock --"
			assert.Equal(t, strings.Contains(a, fwd), mode.net == netProxy)
			if mode.net == netProxy {
				assert.ContainsString(t, a, "--ro-bind /store/pats-static /run/pats/pats")
				assert.ContainsString(t, agent.stdin, "export HTTPS_PROXY='http://127.0.0.1:8080'")
			}
			r := argString(root.args)
			assert.ContainsString(t, r, "--uid 0 --gid 0 --cap-add ALL")
			assert.ContainsString(t, r, "--ro-bind /cache/pats-bwrap-1/apt.conf /run/pats/apt.conf")
			assert.ContainsString(t, root.stdin, "export APT_CONFIG='/run/pats/apt.conf'")
		})
	}
}

// a cancelled context before the start is a launch error; the session runs
// one exec at a time and refuses one after close.
func TestBwrapExecLifecycle(t *testing.T) {
	f := &fakeBwrap{code: 3}
	s := bwrapSessionOn(f, netNone)
	s.dir = t.TempDir()
	code, err := s.Exec(context.Background(), Cmd{Argv: []string{"false"}}, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, code, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Exec(ctx, Cmd{Argv: []string{"true"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	_, err = s.Exec(context.Background(), Cmd{Argv: []string{"true"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
	_, statErr := os.Stat(s.dir)
	assert.That(t, os.IsNotExist(statErr), "close left the session dir")
}

func TestBwrapVersion(t *testing.T) {
	assert.NoError(t, bwrapVersionOK("bubblewrap 0.11.1\n"))
	assert.NoError(t, bwrapVersionOK("bubblewrap 1.0.0\n"))
	assert.Error(t, bwrapVersionOK("bubblewrap 0.9.0\n"), "bwrap 0.9.0 is too old: 0.10 or later is needed")
	assert.Error(t, bwrapVersionOK("nope"), "no version")
}

// a path in the rootfs resolves through its own links, never out of it.
func TestInRootfs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(root+"/etc/ssl/certs", 0o755))
	require.NoError(t, os.MkdirAll(root+"/usr/share/ca", 0o755))
	require.NoError(t, os.WriteFile(root+"/usr/share/ca/bundle", []byte("pem"), 0o644))
	require.NoError(t, os.Symlink("/usr/share/ca/bundle", root+"/etc/ssl/certs/ca-certificates.crt"))
	p, err := inRootfs(root, "/etc/ssl/certs/ca-certificates.crt")
	require.NoError(t, err)
	assert.Equal(t, p, root+"/usr/share/ca/bundle")
	require.NoError(t, os.Symlink("../../../../../../etc/passwd", root+"/etc/up"))
	p, err = inRootfs(root, "/etc/up")
	if err == nil {
		assert.That(t, strings.HasPrefix(p, root), "%s left the rootfs", p)
	}
}

// preflight at the start: off linux the driver says so; on linux a bwrap
// that is missing or older than 0.10 is a launch error naming what is
// needed.
func TestBwrapStartPreflight(t *testing.T) {
	sb := func(r runner) Sandbox {
		d := newBwrap(testProcess(t), Options{Driver: BwrapDriver})
		d.r = r
		return &bwrapSandbox{d: d, image: "sha256:" + strings.Repeat("c", 64)}
	}
	if runtime.GOOS != "linux" {
		_, err := sb(&fakeBwrap{}).Start(context.Background(), Spec{Workdir: t.TempDir()})
		assert.ErrorIs(t, err, ErrLaunch)
		assert.Error(t, err, "runs on linux only")
		return
	}
	old := runFunc(func(ctx context.Context, _ io.Reader, stdout, _ io.Writer, args ...string) error {
		io.WriteString(stdout, "bubblewrap 0.9.0\n")
		return nil
	})
	_, err := sb(old).Start(context.Background(), Spec{Workdir: t.TempDir()})
	assert.ErrorIs(t, err, ErrLaunch)
	assert.Error(t, err, "0.10 or later is needed")
	missing := runFunc(func(context.Context, io.Reader, io.Writer, io.Writer, ...string) error {
		return exec.ErrNotFound
	})
	_, err = sb(missing).Start(context.Background(), Spec{Workdir: t.TempDir()})
	assert.Error(t, err, "bwrap 0.10 or later is needed on PATH")
}

// an exec queued behind another, whose context ends while it waits, never
// starts bwrap.
func TestBwrapQueuedExecCancelled(t *testing.T) {
	f := &fakeBwrap{}
	s := bwrapSessionOn(f, netNone)
	// live when the exec is asked for, ended by the time it has the session:
	// the check before the wait would pass a test that only cancelled early.
	ctx := &endsOnSecondLook{Context: context.Background()}
	_, err := s.Exec(ctx, Cmd{Argv: []string{"true"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, len(f.calls), 0)
}

// endsOnSecondLook is a context that is live the first time its Err is
// asked for and cancelled from then on.
type endsOnSecondLook struct {
	context.Context
	looks atomic.Int32
}

func (c *endsOnSecondLook) Err() error {
	if c.looks.Add(1) > 1 {
		return context.Canceled
	}
	return nil
}
