//go:build !nobwrap

package sandbox

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// a session's root is its image with each harness tree over it, lowest
// first, under the session's own writes.
func TestBwrapArgvStacksTrees(t *testing.T) {
	f := &fakeBwrap{}
	s := bwrapSessionOn(f, netNone)
	s.trees = []string{"/store/harness/aaa", "/store/harness/bbb"}
	_, err := s.Exec(context.Background(), Cmd{Argv: []string{"agent"}}, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.ContainsString(t, argString(f.calls[0].args),
		"--overlay-src /store/rootfs/sha256-x --overlay-src /store/harness/aaa --overlay-src /store/harness/bbb "+
			"--overlay /cache/pats-bwrap-1/upper /cache/pats-bwrap-1/work /")
}

// a cli's tree is unpacked into the store once, under its id, and is there
// whole or not at all.
func TestBwrapPlaceTrees(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := &bwrapDriver{r: &fakeBwrap{}}
	file := tarGz(t, tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\necho 'tool 1.2.3'\n"})
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "amd64", SHA256: "aa", Bin: "package/tool"}
	st, err := d.store()
	require.NoError(t, err)
	assert.Equal(t, d.MissingTree([]string{a.TreeID()}), a.TreeID())

	ids, err := d.PlaceTrees(context.Background(), []Placed{{Artefact: a, File: file}})
	require.NoError(t, err)
	assert.EqualArrays(t, ids, []string{a.TreeID()})
	assert.Equal(t, d.MissingTree(ids), "")
	target, err := os.Readlink(st.harnessTree(a.TreeID()) + "/usr/local/bin/tool")
	require.NoError(t, err)
	assert.Equal(t, target, "/opt/pats/harness/tool/package/tool")
	left, err := os.ReadDir(st.harnessTree(a.TreeID()) + "/..")
	require.NoError(t, err)
	for _, e := range left {
		assert.That(t, !strings.HasPrefix(e.Name(), ".new-"), "a half-made tree was left: %s", e.Name())
	}

	// there already: nothing is unpacked, the artefact is not even read.
	again, err := d.PlaceTrees(context.Background(), []Placed{{Artefact: a, File: "/nonexistent/not-read"}})
	require.NoError(t, err)
	assert.EqualArrays(t, again, ids)

	// one that cannot be made leaves no tree under its id.
	bad := Artefact{CLI: "other", Version: "1", Arch: "amd64", SHA256: "bb", Bin: "package/nope"}
	_, err = d.PlaceTrees(context.Background(), []Placed{{Artefact: bad, File: file}})
	assert.Error(t, err, "other 1: ")
	assert.Equal(t, d.MissingTree([]string{bad.TreeID()}), bad.TreeID())
}

// clean --all takes the fetched artefacts and the trees unpacked from them: a
// tree a sandbox holds is kept and said so, a fetch in flight keeps its temp
// file, and a dry run removes nothing.
func TestCleanHarness(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := &bwrapDriver{r: &fakeBwrap{}}
	file := tarGz(t, tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\n"})
	held := Artefact{CLI: "tool", Version: "1", Arch: "amd64", SHA256: "aa", Bin: "package/tool"}
	free := Artefact{CLI: "tool", Version: "2", Arch: "amd64", SHA256: "bb", Bin: "package/tool"}
	_, err := d.PlaceTrees(context.Background(), []Placed{{Artefact: held, File: file}, {Artefact: free, File: file}})
	require.NoError(t, err)
	st, err := d.store()
	require.NoError(t, err)
	crashed := st.harnessTree(free.TreeID()) + ".new" // what a crashed unpack left
	require.NoError(t, os.MkdirAll(crashed+"/opt", 0o755))
	unlock, err := lockAt(context.Background(), st.treeLock(held.TreeID()), false, false)
	require.NoError(t, err)

	cache, err := artefactCache()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(cache, 0o755))
	for _, n := range []string{"aa", "fetch-fresh", "fetch-stale"} {
		require.NoError(t, os.WriteFile(cache+"/"+n, []byte("x"), 0o644))
	}
	old := time.Now().Add(-2 * fetchGrace)
	require.NoError(t, os.Chtimes(cache+"/fetch-stale", old, old))

	var events []string
	report := func(e CleanEvent) { events = append(events, e.String()) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	require.NoError(t, CleanHarness(true, report))
	assert.That(t, exists(cache+"/aa") && d.MissingTree([]string{free.TreeID()}) == "", "a dry run removed something")
	assert.ContainsString(t, strings.Join(events, "\n"), "would remove harness artefact "+cache+"/aa")

	events = nil
	require.NoError(t, CleanHarness(false, report))
	all := strings.Join(events, "\n")
	assert.That(t, !exists(cache+"/aa") && !exists(cache+"/fetch-stale"), "an artefact was left")
	assert.That(t, exists(cache+"/fetch-fresh"), "a fetch in flight lost its file")
	assert.Equal(t, d.MissingTree([]string{held.TreeID()}), "")
	assert.Equal(t, d.MissingTree([]string{free.TreeID()}), free.TreeID())
	assert.That(t, !exists(crashed), "a crashed unpack's dir was left")
	assert.ContainsString(t, all, "removed harness tree "+st.harnessTree(free.TreeID()))
	assert.ContainsString(t, all, "host: kept tree "+st.harnessTree(held.TreeID())+" (in use by a running sandbox, or being unpacked)")

	unlock()
	require.NoError(t, CleanHarness(false, report))
	assert.Equal(t, d.MissingTree([]string{held.TreeID()}), held.TreeID())
}
