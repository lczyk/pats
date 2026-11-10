package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// fakeLXC records every lxc call and answers the few whose output matters.
type fakeLXC struct {
	callLog
	devices map[string]map[string]string // the expanded devices a query returns
	config  map[string]string
	failOn  string
}

func (f *fakeLXC) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.record(args)
	joined := strings.Join(args, " ")
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		io.WriteString(stderr, "Error: fake failure")
		return &fakeExit{}
	}
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	switch {
	case args[0] == "remote" && args[1] == "get-default":
		io.WriteString(stdout, "srv\n")
	case args[0] == "profile" && args[1] == "device":
		io.WriteString(stdout, "default\n")
	case args[0] == "query":
		b, _ := json.Marshal(map[string]any{"expanded_devices": f.devices, "expanded_config": f.config})
		stdout.Write(b)
	case args[0] == "exec" && strings.Contains(joined, "id -u pats"):
		io.WriteString(stdout, "1001\n")
	case args[0] == "exec" && strings.Contains(joined, "pats egress run"):
		io.WriteString(stderr, "listening fake\n")
		<-ctx.Done()
		return ctx.Err()
	case args[0] == "exec" && strings.Contains(joined, "tar -c"):
		tw := tar.NewWriter(stdout)
		tw.WriteHeader(&tar.Header{Name: "out.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
		tw.Write([]byte("hi\n"))
		tw.Close()
	}
	return nil
}

type fakeExit struct{}

func (fakeExit) Error() string { return "exit status 1" }

func fakeLXD(t *testing.T, o Options, s ...SandboxOptions) (*lxd, *fakeLXC) {
	f := &fakeLXC{}
	l := newLXD(testProcess(t), runFunc(f.run), o)
	if len(s) > 0 {
		l = l.on(s[0])
	}
	return l, f
}

// an open sandbox: made stopped from the image (on its own server) in the pats project on the
// pinned remote, stamped as this run's, its own bridge and acl in the default
// project, the expanded config checked before start, the workdir pushed.
func TestLXDStartOpen(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "pats-img-x", Limits: Limits{CPU: 2, Memory: "4GiB", Disk: "10GiB"}})
	work := t.TempDir()
	sess, err := l.Start(context.Background(), Spec{Workdir: work})
	require.NoError(t, err)
	s := sess.(*lxdSession)
	f.devices = map[string]map[string]string{}

	init := strings.Join(f.call("init"), " ")
	for _, want := range []string{"init srv:pats-img-x srv:" + s.name, "--project pats", "--storage default",
		"user.pats.run=" + l.p.token, "limits.cpu=2", "limits.memory=4GiB", "root,size=10GiB"} {
		assert.ContainsString(t, init, want)
	}
	acl := strings.Join(f.call("network acl rule add"), " ")
	assert.ContainsString(t, acl, "action=reject")
	assert.ContainsString(t, acl, "192.168.0.0/16")
	assert.ContainsString(t, acl, "--project default")
	net := strings.Join(f.call("network create"), " ")
	assert.ContainsString(t, net, "ipv4.nat=true")
	assert.ContainsString(t, net, "security.acls.default.egress.action=allow")
	assert.ContainsString(t, strings.Join(f.call("config device add"), " "), "network="+s.bridge)

	// the device check runs before the start.
	q, st := -1, -1
	for i, c := range f.all() {
		switch strings.Fields(c)[0] {
		case "query":
			q = i
		case "start":
			st = i
		}
	}
	assert.That(t, q >= 0 && q < st, "expanded config not checked before start")
	assert.NotNil(t, f.call("exec srv:"+s.name+" --project pats --force-noninteractive --user 0 --group 0 --cwd / -- sh -c mkdir"))
	assert.Equal(t, s.uid, "1001")

	require.NoError(t, sess.Close())
	assert.NotNil(t, f.call("delete srv:"+s.name))
	assert.NotNil(t, f.call("network delete srv:"+s.bridge))
	assert.NotNil(t, f.call("network acl delete srv:"+s.bridge))
}

// a nic a profile adds is refused, and so is raw config, before anything
// starts; under egress none the instance gets no nic at all.
func TestLXDStartRefusesForeignNic(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img", Profiles: []string{"extra"}})
	f.devices = map[string]map[string]string{"eth9": {"type": "nic", "network": "lxdbr0"}}
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "would bypass the sandbox's egress")
	assert.Nil(t, f.call("start"))
	assert.Nil(t, f.call("network create"))
	assert.NotNil(t, f.call("delete"))
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "-p extra")

	l, f = fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
	f.config = map[string]string{"raw.lxc": "lxc.net.0.type=veth"}
	_, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "raw config")
}

// the agent runs as the pats user, root steps as root, both with the egress
// env under their own. a vm carries agent.nic_config.
func TestLXDExecArgs(t *testing.T) {
	s := &lxdSession{l: &lxd{o: Options{Remote: "srv"}}, name: "pats-x", uid: "1001", env: map[string]string{"A": "1", "B": "2"}}
	// the env goes in on stdin, never on lxc's command line, where ps reads it.
	argv, in := s.execArgs(Cmd{Argv: []string{"run", "it"}, Env: map[string]string{"B": "it's 3"}}, nil)
	args := strings.Join(argv, " ")
	wrap := strings.Join(envArgv(nil), " ")
	assert.Equal(t, args, "exec srv:pats-x --project pats --force-noninteractive --user 1001 --group 1001 --cwd /workspace -- "+wrap+" run it")
	script, _ := io.ReadAll(in)
	assert.Equal(t, string(script), "export A='1'\nexport B='it'\\''s 3'\n")
	argv, in = s.execArgs(Cmd{Argv: []string{"id"}, Root: true, Dir: "/"}, nil)
	assert.Equal(t, strings.Join(argv, " "), "exec srv:pats-x --project pats --force-noninteractive --user 0 --group 0 --cwd / -- "+wrap+" id")
	assert.That(t, in != nil, "no env on stdin")
	// a step streaming its own stdin carries the egress env as flags.
	argv, in = s.execArgs(Cmd{Argv: []string{"tar"}, Root: true, Dir: "/"}, strings.NewReader("data"))
	assert.Equal(t, strings.Join(argv, " "), "exec srv:pats-x --project pats --force-noninteractive --user 0 --group 0 --cwd / --env A=1 --env B=2 -- tar")
	b, _ := io.ReadAll(in)
	assert.Equal(t, string(b), "data")

	l, f := fakeLXD(t, Options{Driver: "lxd", VM: true}, SandboxOptions{Image: "img"})
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "--vm -c agent.nic_config=true")
}

// HandOver writes the sudoers rule only when the pair allows sudo.
func TestLXDHandOverSudo(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}, Sudo: sudo})
		require.NoError(t, err)
		f.reset()
		require.NoError(t, sess.HandOver(context.Background(), WorkMount))
		joined := strings.Join(f.all(), "\n")
		assert.ContainsString(t, joined, "chown -R 1001:1001")
		assert.Equal(t, strings.Contains(joined, "NOPASSWD"), sudo)
		assert.Equal(t, strings.Contains(joined, "rm -f /etc/sudoers.d/pats"), !sudo)
	}
}

// Sync replaces each writable bind's host copy with what is in the instance.
func TestLXDSync(t *testing.T) {
	l, _ := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
	work, ro := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "stale"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ro, "keep"), []byte("x"), 0o644))
	sess, err := l.Start(context.Background(), Spec{Workdir: work, Egress: Egress{None: true},
		Mounts: []Mount{{Host: ro, Container: "/ro", ReadOnly: true}}})
	require.NoError(t, err)
	require.NoError(t, sess.Sync(context.Background()))
	b, err := os.ReadFile(filepath.Join(work, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "hi\n")
	_, err = os.Stat(filepath.Join(work, "stale"))
	assert.That(t, os.IsNotExist(err), "a file the agent removed survived the sync")
	_, err = os.Stat(filepath.Join(ro, "keep"))
	assert.NoError(t, err)
}

// a tar entry may not leave the destination, and nothing is written, made or
// removed through a symlink: one the stream itself made, or one already
// there. whatever the archive, what is beside the destination stays as it was.
func TestReadTarSafety(t *testing.T) {
	type entry struct {
		name, link string
		typ        byte
	}
	archive := func(es ...entry) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range es {
			h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o644}
			if e.typ == tar.TypeReg {
				h.Size = 1
			}
			require.NoError(t, tw.WriteHeader(h))
			if e.typ == tar.TypeReg {
				_, err := tw.Write([]byte("x"))
				require.NoError(t, err)
			}
		}
		require.NoError(t, tw.Close())
		return &buf
	}
	for name, c := range map[string]struct {
		before string // a symlink to outside already in the destination, "" for none
		es     func(outside string) []entry
	}{
		"a name that climbs":       {es: func(string) []entry { return []entry{{"../victim", "", tar.TypeReg}} }},
		"an absolute name":         {es: func(o string) []entry { return []entry{{o + "/victim", "", tar.TypeReg}} }},
		"a file through its link":  {es: func(o string) []entry { return []entry{{"a", o, tar.TypeSymlink}, {"a/victim", "", tar.TypeReg}} }},
		"a link through its link":  {es: func(o string) []entry { return []entry{{"a", o, tar.TypeSymlink}, {"a/victim", "x", tar.TypeSymlink}} }},
		"a dir through its link":   {es: func(o string) []entry { return []entry{{"a", o, tar.TypeSymlink}, {"a/d", "", tar.TypeDir}} }},
		"a hard link that climbs":  {es: func(string) []entry { return []entry{{"h", "../outside/victim", tar.TypeLink}} }},
		"a hard link to a link":    {before: "a", es: func(string) []entry { return []entry{{"h", "a/victim", tar.TypeLink}} }},
		"a file through a link":    {before: "a", es: func(string) []entry { return []entry{{"a/victim", "", tar.TypeReg}} }},
		"a dir over a link":        {before: "a", es: func(string) []entry { return []entry{{"a", "", tar.TypeDir}} }},
		"a file over a link":       {before: "victim", es: func(string) []entry { return []entry{{"victim", "", tar.TypeReg}} }},
		"a link through a link":    {before: "a", es: func(string) []entry { return []entry{{"a/victim", "x", tar.TypeSymlink}} }},
		"a hard link over a link":  {before: "a", es: func(string) []entry { return []entry{{"f", "", tar.TypeReg}, {"a/victim", "f", tar.TypeLink}} }},
		"a link out, left a link":  {es: func(o string) []entry { return []entry{{"a", o, tar.TypeSymlink}} }},
		"a file, then its own dir": {es: func(string) []entry { return []entry{{"a", "", tar.TypeReg}, {"a/b", "", tar.TypeReg}} }},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir, outside := filepath.Join(base, "dest"), filepath.Join(base, "outside")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.MkdirAll(outside, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "victim"), []byte("precious"), 0o640))
			if c.before != "" {
				target := outside
				if c.before == "victim" {
					target = filepath.Join(outside, "victim")
				}
				require.NoError(t, os.Symlink(target, filepath.Join(dir, c.before)))
			}

			_ = readTar(archive(c.es(outside)...), dir) // refusing the archive is fine

			got, err := os.ReadFile(filepath.Join(outside, "victim"))
			require.NoError(t, err)
			assert.Equal(t, string(got), "precious")
			fi, err := os.Lstat(filepath.Join(outside, "victim"))
			require.NoError(t, err)
			assert.Equal(t, fi.Mode(), os.FileMode(0o640))
			fi, err = os.Lstat(outside)
			require.NoError(t, err)
			assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o750))
			ents, err := os.ReadDir(outside)
			require.NoError(t, err)
			assert.Len(t, ents, 1, "an entry was made beside the destination's")
			ents, err = os.ReadDir(base)
			require.NoError(t, err)
			assert.Len(t, ents, 2)
		})
	}

	// the refusals say so.
	assert.ErrorIs(t, readTar(archive(entry{"../escape", "", tar.TypeReg}), t.TempDir()), errEscape)
	outside := t.TempDir()
	err := readTar(archive(entry{"a", outside, tar.TypeSymlink}, entry{"a/victim", "x", tar.TypeSymlink}), t.TempDir())
	assert.ErrorIs(t, err, errEscape)
}

// the destination may itself be reached through a symlink, as a project dir
// under a linked home is: it is what lies under it that no link may stand in.
func TestReadTarIntoLinkedDir(t *testing.T) {
	real := t.TempDir()
	dir := filepath.Join(t.TempDir(), "linked")
	require.NoError(t, os.Symlink(real, dir))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./d/f", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./top", Linkname: "d/f", Typeflag: tar.TypeSymlink}))
	require.NoError(t, tw.Close())
	require.NoError(t, readTar(&buf, dir))
	got, err := os.ReadFile(filepath.Join(real, "top"))
	require.NoError(t, err)
	assert.Equal(t, string(got), "x")
}

// a second name of a symlink is that symlink again, one of what does not
// travel (a fifo) is left out with it, and neither fails the pull. a name
// linked to itself is the file it is.
func TestReadTarHardLinkToOthers(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("keep"))
	require.NoError(t, err)
	for _, h := range []tar.Header{
		{Name: "ln", Linkname: "f", Typeflag: tar.TypeSymlink},
		{Name: "ln2", Linkname: "ln", Typeflag: tar.TypeLink},
		{Name: "fifo", Typeflag: tar.TypeFifo},
		{Name: "fifo2", Linkname: "fifo", Typeflag: tar.TypeLink},
		{Name: "f", Linkname: "f", Typeflag: tar.TypeLink},
	} {
		require.NoError(t, tw.WriteHeader(&h))
	}
	require.NoError(t, tw.Close())
	dir := t.TempDir()
	require.NoError(t, readTar(&buf, dir))
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, string(got), "keep")
	for _, name := range []string{"ln", "ln2"} {
		target, err := os.Readlink(filepath.Join(dir, name))
		require.NoError(t, err, name)
		assert.Equal(t, target, "f")
	}
	for _, name := range []string{"fifo", "fifo2"} {
		_, err := os.Lstat(filepath.Join(dir, name))
		assert.That(t, os.IsNotExist(err), "%s travelled", name)
	}
}

// a hard link is to a file the stream itself brought: one already in the
// destination may be another name of a file outside it.
func TestReadTarHardLinkToWhatWasThere(t *testing.T) {
	base := t.TempDir()
	dir, victim := filepath.Join(base, "dest"), filepath.Join(base, "victim")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(victim, []byte("precious"), 0o640))
	require.NoError(t, os.Link(victim, filepath.Join(dir, "x")))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "y", Linkname: "x", Typeflag: tar.TypeLink}))
	require.NoError(t, tw.Close())
	assert.Error(t, readTar(&buf, dir), "no file of this archive")
	_, err := os.Lstat(filepath.Join(dir, "y"))
	assert.That(t, os.IsNotExist(err), "a second name was made for a file the stream did not bring")
}

// a file with two names in the instance comes home with both, one file still.
func TestReadTarHardLink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./d/first", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./second", Linkname: "./d/first", Typeflag: tar.TypeLink}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./third", Linkname: "./nowhere", Typeflag: tar.TypeLink}))
	require.NoError(t, tw.Close())
	dir := t.TempDir()
	assert.Error(t, readTar(&buf, dir), "no file of this archive")
	first, err := os.Stat(filepath.Join(dir, "d", "first"))
	require.NoError(t, err)
	second, err := os.Stat(filepath.Join(dir, "second"))
	require.NoError(t, err)
	assert.That(t, os.SameFile(first, second), "the second name is another file")
}

// what writeTar packs, readTar unpacks: files, modes, dirs and symlinks.
func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "d", "x.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink("d/x.sh", filepath.Join(src, "ln")))
	fi, err := os.Stat(src)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, src, fi))
	dst := t.TempDir()
	require.NoError(t, readTar(&buf, dst))
	st, err := os.Stat(filepath.Join(dst, "d", "x.sh"))
	require.NoError(t, err)
	assert.Equal(t, st.Mode().Perm(), os.FileMode(0o755))
	target, err := os.Readlink(filepath.Join(dst, "ln"))
	require.NoError(t, err)
	assert.Equal(t, target, "d/x.sh")
}

// a socket or a fifo in a pushed dir (an agent's home collects them) is left
// out rather than failing the push.
func TestWriteTarSkipsSpecialFiles(t *testing.T) {
	src, err := os.MkdirTemp("", "pt") // short: a socket path has a length cap
	require.NoError(t, err)
	defer os.RemoveAll(src)
	require.NoError(t, os.WriteFile(filepath.Join(src, "keep"), []byte("x"), 0o644))
	ln, err := net.Listen("unix", filepath.Join(src, "sock"))
	require.NoError(t, err)
	defer ln.Close()
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644))
	fi, err := os.Stat(src)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, writeTar(&buf, src, fi))
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, h.Name)
	}
	assert.Equal(t, strings.Join(names, ","), "keep")
}

// when the local tar fails, the instance's tar sees a cut stream and
// complains; the push error leads with the local cause.
func TestLXDPushReportsLocalError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "tar -x") {
			_, _ = io.Copy(io.Discard, stdin)
			io.WriteString(stdout, "tar: Unexpected EOF in archive")
			return &fakeExit{}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "secret"), []byte("x"), 0o000))
	s := &lxdSession{l: l, name: "pats-x"}
	err := s.Push(context.Background(), src, "/home/pats")
	require.Error(t, err, "permission denied")
	assert.ContainsString(t, err.Error(), "in the instance: tar: Unexpected EOF")
}

func TestLXDName(t *testing.T) {
	assert.Equal(t, lxdName("/x/pats-work-123"), "pats-work-123")
	assert.Equal(t, lxdName("/x/Odd_Name.1"), "pats-odd-name-1")
	long := lxdName("/x/" + strings.Repeat("a", 80))
	assert.That(t, len(long) <= 63, "name too long: %d", len(long))
	l := newLXD(newProcess(wallClock{}, defaultTimings), nil, Options{})
	assert.That(t, len(l.bridgeName("pats-work-123")) <= 15, "bridge name too long")
}

// a proxied sandbox: an addressless bridge with a reject acl, the agent on it
// at a static pool address, the canary before the proxy, the proxy container
// on the bridge and the uplink with forwarding off, listening on the bridge
// address only, and the agent told where it is.
func TestLXDStartProxy(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	l, f := fakeLXD(t, Options{Driver: "lxd", ProxyBin: bin, Uplink: "up0"}, SandboxOptions{Image: "img"})
	answers := map[string]string{
		"egress-canary":        "egress-canary leak=0\n",
		"egress-reach":         "egress-reach ok=1\n",
		"id -u patsproxy":      "999\n",
		"image list":           `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`,
		"/1.0/images/aliases/": `{"target":"abc"}`,
		"query srv:/1.0 ":      `{"environment":{"architectures":["x86_64"]}}`,
	}
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ") + " "
		for k, v := range answers {
			if strings.Contains(joined, k) {
				f.record(args)
				io.WriteString(stdout, v)
				return nil
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, SelfTest: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	require.NoError(t, err)
	s := sess.(*lxdSession)

	net := strings.Join(f.call("network create"), " ")
	for _, want := range []string{"ipv4.address=none", "ipv6.address=none", "security.acls.default.egress.action=reject", "security.acls.default.ingress.action=reject"} {
		assert.ContainsString(t, net, want)
	}
	agentIP := s.egress.agentIP.String()
	proxyIP := s.egress.ip.String()
	var rules []string
	for _, j := range f.all() {
		if strings.Contains(j, "network acl rule add") {
			rules = append(rules, j)
		}
	}
	// one rule, agent to proxy port: both defaults reject the rest.
	assert.Equal(t, len(rules), 1)
	assert.ContainsString(t, rules[0], "egress --project default action=allow source="+agentIP+"/32 destination="+proxyIP+"/32 protocol=tcp destination_port=8080")
	assert.That(t, testPool.Contains(s.egress.ip), "proxy %s outside the pool", proxyIP)
	all := strings.Join(f.all(), "\n")
	canary := strings.Index(all, "egress-canary")
	proxyInit := strings.Index(all, "init srv:pats-img-")
	assert.That(t, canary >= 0 && canary < proxyInit, "canary not run before the proxy exists")
	assert.ContainsString(t, all, "ip addr replace "+agentIP+"/29 dev pats0")
	assert.ContainsString(t, all, "network=up0 name=uplink0")
	assert.ContainsString(t, all, "net.ipv4.ip_forward=0")
	// the proxy runs as its own user, its policy and ca key in on stdin:
	// never on lxc's command line, where ps reads it.
	assert.ContainsString(t, all, "--user 999 --group 999 --cwd / -- /bin/sh -c")
	assert.That(t, !strings.Contains(all, "PATS_EGRESS_"), "proxy env on the command line:\n%s", all)
	assert.Equal(t, s.env["HTTP_PROXY"], "http://"+proxyIP+":8080")

	require.NoError(t, sess.Close())
	all = strings.Join(f.all(), "\n")
	assert.ContainsString(t, all, "delete srv:"+s.name+"-p")
	assert.ContainsString(t, all, "network delete srv:"+s.bridge)
}

// clean groups by run token: a run whose marker's lease is stale goes,
// instances before bridges before acls before the marker; a run with a fresh
// lease stays, forced or not, unless named; a run with no marker goes once all
// of it is old; cached images stay always, and so does an image a run built
// whose alias a forced init re-pointed away (a pin may name it); one under a
// run's private alias alone goes once unused for 30 days.
func TestLXDClean(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old, fresh := now.Add(-time.Hour).Unix(), now.Add(-time.Minute).Unix()
	cfg := func(tok string, created int64) map[string]string {
		return map[string]string{"user.pats.run": tok, "user.pats.created": strconv.FormatInt(created, 10)}
	}
	marker := func(tok string, lease int64) map[string]any {
		c := cfg(tok, old)
		c["user.pats.lease"] = strconv.FormatInt(lease, 10)
		return map[string]any{"name": "pats-run-" + tok, "config": c}
	}
	answers := map[string]any{
		"/1.0/instances": []map[string]any{
			{"name": "pats-dead", "config": cfg("dead", old)},
			{"name": "pats-live", "config": cfg("live", old)},
			{"name": "pats-orphan", "config": cfg("orphan", old)},
			{"name": "pats-young", "config": cfg("young", fresh)},
			{"name": "user-thing", "config": map[string]string{}},
		},
		"/1.0/networks": []map[string]any{
			{"name": "patsbrdead", "config": cfg("dead", old)},
			{"name": "lxdbr0", "config": map[string]string{}},
			{"name": "patsup", "config": map[string]string{}},
		},
		"/1.0/network-acls": []map[string]any{
			{"name": "patsbrdead", "config": cfg("dead", old)},
			marker("dead", old),
			marker("live", fresh),
		},
		"/1.0/images": []map[string]any{
			{"fingerprint": "aaaaaaaaaaaaaaaa", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-k"}}},
			{"fingerprint": "bbbbbbbbbbbbbbbb", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-k-dead"}}},
			{"fingerprint": "cccccccccccccccc", "properties": cfg("dead", old), "aliases": []map[string]string{}},
			{"fingerprint": "dddddddddddddddd", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-d-dead"}},
				"uploaded_at": now.Add(-24 * time.Hour), "last_used_at": "0001-01-01T00:00:00Z"},
			{"fingerprint": "eeeeeeeeeeeeeeee", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-e"}, {"name": "pats-img-e-dead"}}},
			{"fingerprint": "ffffffffffffffff", "properties": cfg("beef", fresh), "aliases": []map[string]string{{"name": "pats-img-f"}, {"name": "pats-img-f-beef"}}},
		},
	}
	var calls []string
	l := newLXD(testProcess(t), nil, Options{Remote: "srv"})
	l.p.clock = fixedClock{now: now}
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "query" {
			for k, v := range answers {
				if strings.Contains(args[1], k+"?") {
					b, _ := json.Marshal(v)
					stdout.Write(b)
				}
			}
		}
		if args[0] == "network" && args[1] == "show" {
			return &fakeExit{}
		}
		return nil
	})
	var out bytes.Buffer
	_, err := l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", Report: TextReport(&out)})
	require.NoError(t, err)
	log := strings.Join(calls, "\n")
	for _, want := range []string{"delete srv:pats-dead", "network delete srv:patsbrdead", "network acl delete srv:patsbrdead",
		"image delete srv:bbbbbbbbbbbbbbbb", "network acl delete srv:pats-run-dead", "delete srv:pats-orphan"} {
		assert.ContainsString(t, log, want)
	}
	for _, keep := range []string{"srv:pats-live", "srv:pats-young", "srv:user-thing", "srv:lxdbr0", "srv:patsup", "srv:aaaaaaaaaaaaaaaa",
		"srv:cccccccccccccccc", "srv:dddddddddddddddd"} {
		assert.Equal(t, strings.Contains(log, "delete "+keep), false, "%s removed", keep)
	}
	assert.That(t, strings.Index(log, "delete srv:pats-dead") < strings.Index(log, "network delete srv:patsbrdead"), "bridge before its instance")
	assert.That(t, strings.Index(log, "network acl delete srv:patsbrdead") < strings.Index(log, "network acl delete srv:pats-run-dead"), "marker before the rest")
	assert.ContainsString(t, out.String(), "left 3 live run(s)")

	// a dry run says what the real one then does.
	var dry, real bytes.Buffer
	_, err = l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", All: true, DryRun: true, Report: TextReport(&dry)})
	require.NoError(t, err)
	_, err = l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", All: true, Report: TextReport(&real)})
	require.NoError(t, err)
	assert.Equal(t, dry.String(), strings.ReplaceAll(real.String(), "removed", "would remove"))
	assert.ContainsString(t, real.String(), "removed lxd srv image pats-img-k (cached)")
	// its dead run's private alias goes first, then it is a cached image; a
	// live run's alias keeps one the run's.
	assert.ContainsString(t, real.String(), "removed lxd srv alias pats-img-e-dead (run dead: its lease is stale)\nremoved lxd srv")
	assert.ContainsString(t, real.String(), "removed lxd srv image pats-img-e (cached)")
	assert.That(t, !strings.Contains(real.String(), "pats-img-f"), "took a live run's image:\n%s", real.String())

	calls = nil
	_, err = l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", All: true, Report: TextReport(&out)})
	require.NoError(t, err)
	log = strings.Join(calls, "\n")
	assert.Equal(t, strings.Contains(log, "delete srv:pats-live"), false, "--all took a live run")
	// a live run stays on the shared uplink, so it stays too.
	assert.Equal(t, strings.Contains(log, "network delete srv:patsup"), false, "--all took the uplink under a live run")
	assert.ContainsString(t, out.String(), "lxd srv: kept network patsup (the shared uplink) (a run left here may use it)")
	// --all takes the cached images too, and a run's own image however young.
	for _, fp := range []string{"aaaaaaaaaaaaaaaa", "cccccccccccccccc", "dddddddddddddddd"} {
		assert.ContainsString(t, log, "image delete srv:"+fp)
	}
	calls = nil
	_, err = l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", Force: true, Report: TextReport(&out)})
	require.NoError(t, err)
	log = strings.Join(calls, "\n")
	assert.ContainsString(t, log, "delete srv:pats-live")
	up := strings.Index(log, "network delete srv:patsup")
	require.That(t, up >= 0, "--force left the shared uplink:\n%s", log)
	assert.That(t, up > strings.LastIndex(log, "delete srv:pats-"), "the uplink went before an instance on it")
	calls = nil
	_, err = l.p.clean(context.Background(), l, CleanOptions{Place: "lxd srv", Run: "live", Report: TextReport(&out)})
	require.NoError(t, err)
	log = strings.Join(calls, "\n")
	assert.ContainsString(t, log, "delete srv:pats-live")
	assert.Equal(t, strings.Contains(log, "delete srv:pats-young"), false, "--run took another run")
}

// a build on a miss: progress per step, a low pool warned about, and a
// builder with no internet failing fast with the docker hint, before any
// provisioning runs. a builder the cleanup could not delete is named.
func TestLXDImageBuild(t *testing.T) {
	run := func(t *testing.T, offline, keepBuilder bool) (*fakeLXC, []string, []string, error) {
		l, f := fakeLXD(t, Options{Driver: "lxd", Uplink: "up0"})
		inner := f.run
		l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			joined := strings.Join(args, " ")
			switch {
			case joined == "query srv:/1.0":
				f.record(args)
				io.WriteString(stdout, `{"environment":{"architectures":["x86_64"]}}`)
				return nil
			case strings.HasPrefix(joined, "image list ubuntu: 26.04"):
				f.record(args)
				io.WriteString(stdout, `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`)
				return nil
			case strings.HasPrefix(joined, "query srv:/1.0/images/aliases/pats-img-"):
				f.record(args)
				if strings.Contains(joined, "-"+l.p.token+"?") {
					io.WriteString(stdout, `{"target":"def"}`) // the run-private alias, published
					return nil
				}
				return &fakeExit{}
			case strings.Contains(joined, "/storage-pools/default/resources"):
				f.record(args)
				io.WriteString(stdout, `{"space":{"used":`+strconv.Itoa(27<<30)+`,"total":`+strconv.Itoa(28<<30)+`}}`)
				return nil
			case offline && strings.Contains(joined, "/dev/tcp/1.1.1.1/443"):
				f.record(args)
				return &fakeExit{}
			case keepBuilder && strings.HasPrefix(joined, "delete srv:pats-build-"):
				f.record(args)
				io.WriteString(stderr, "Error: busy")
				return &fakeExit{}
			}
			return inner(ctx, stdin, stdout, stderr, args...)
		})
		var steps, warns []string
		spec := lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"},
			Progress: func(m string) { steps = append(steps, m) }, Warn: func(m string) { warns = append(warns, m) }}
		_, _, _, _, err := l.image(context.Background(), spec, io.Discard)
		return f, steps, warns, err
	}

	t.Run("offline", func(t *testing.T) {
		f, _, warns, err := run(t, true, false)
		require.Error(t, err, "image builder on bridge up0 does not reach the internet")
		assert.ContainsString(t, err.Error(), "DOCKER-USER")
		assert.ContainsString(t, err.Error(), LXDUplinkEnv)
		for _, c := range f.all() {
			assert.That(t, !strings.Contains(c, "provision true"), "provisioned offline")
		}
		assert.Equal(t, len(warns), 1)
		assert.ContainsString(t, warns[0], "1.0 GiB free of 28.0 GiB")
	})

	t.Run("ok", func(t *testing.T) {
		_, steps, _, err := run(t, false, false)
		require.NoError(t, err)
		assert.Equal(t, len(steps), 4)
		assert.ContainsString(t, steps[0], "building lxd image pats-img-")
		assert.ContainsString(t, steps[1], "launching builder pats-build-")
		assert.ContainsString(t, steps[2], "provisioning: true")
		assert.ContainsString(t, steps[3], "publishing pats-img-")
	})

	t.Run("builder left behind", func(t *testing.T) {
		_, _, warns, err := run(t, true, true)
		require.Error(t, err, "was left behind")
		assert.ContainsString(t, warns[len(warns)-1], "builder pats-build-")
	})
}

func TestLogTail(t *testing.T) {
	assert.Equal(t, LogTail(""), "")
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	b.WriteString(strings.Repeat("x", 500) + "\n")
	got := LogTail(b.String())
	assert.ContainsString(t, got, "line 39")
	assert.That(t, !strings.Contains(got, "line 25\n"), "kept too much: %s", got)
	assert.ContainsString(t, got, strings.Repeat("x", 240)+"...")
}

// a delete that lands while the server is still creating the instance says
// "not found"; it is retried until the create is over, and then removes it.
func TestLXDDeleteInstanceWaitsForCreate(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	busy := 2
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "query srv:/1.0/operations"):
			f.record(args)
			if busy > 0 {
				busy--
				io.WriteString(stdout, `{"running":[{"resources":{"instances":["/1.0/instances/pats-x?project=pats"]}}]}`)
			} else {
				io.WriteString(stdout, `{"running":[]}`)
			}
			return nil
		case strings.HasPrefix(joined, "delete srv:pats-x"):
			f.record(args)
			if busy > 0 {
				io.WriteString(stderr, `Error: Failed to fetch instance "pats-x" in project "pats": Instance not found`)
				return &fakeExit{}
			}
			return nil
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	require.NoError(t, l.deleteInstance("pats-x"))
	deletes := 0
	for _, c := range f.all() {
		if strings.HasPrefix(c, "delete ") {
			deletes++
		}
	}
	assert.Equal(t, deletes, 3)
	assert.Equal(t, busy, 0)
}

// the create finishes right after a delete said not found: the operations
// were read before that delete, so the round is retried and the instance,
// now there, goes (docs/spec/fizz/lxd_delete.fizz).
func TestLXDDeleteInstanceCreateFinishesBetween(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	creating, present := true, false
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "query srv:/1.0/operations"):
			if creating {
				io.WriteString(stdout, `{"running":[{"resources":{"instances":["/1.0/instances/pats-x?project=pats"]}}]}`)
			} else {
				io.WriteString(stdout, `{"running":[]}`)
			}
			return nil
		case strings.HasPrefix(joined, "delete srv:pats-x"):
			if present {
				present = false
				return nil
			}
			if creating { // the server finishes the create just after this
				creating, present = false, true
			}
			io.WriteString(stderr, `Error: Instance not found`)
			return &fakeExit{}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	require.NoError(t, l.deleteInstance("pats-x"))
	assert.That(t, !present, "instance left behind")
}

// the proxy image build writes its steps to the log it is given, as a
// sandbox image build does.
func TestLXDProxyImageProgress(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv", ProxyBin: bin, Uplink: "up0"})
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "image list ubuntu-minimal: 26.04"):
			io.WriteString(stdout, `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`)
			return nil
		case strings.HasPrefix(joined, "query srv:/1.0/images/aliases/pats-img-"):
			if strings.Contains(joined, "-"+l.p.token+"?") {
				io.WriteString(stdout, `{"target":"def"}`) // the run-private alias, published
				return nil
			}
			return &fakeExit{}
		case joined == "query srv:/1.0":
			io.WriteString(stdout, `{"environment":{"architectures":["x86_64"]}}`)
			return nil
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	var log bytes.Buffer
	_, built, err := l.buildProxyImage(context.Background(), false, &log)
	require.NoError(t, err)
	assert.Equal(t, built, true)
	assert.ContainsString(t, log.String(), "==> building lxd image pats-img-")
}

// a fresh client's first command prints a banner before the remote's name.
func TestDefaultRemoteAfterBanner(t *testing.T) {
	l, _ := fakeLXD(t, Options{Driver: "lxd"})
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		io.WriteString(stdout, "To start your first container, try: lxc launch ubuntu:24.04\nOr for a virtual machine: lxc launch ubuntu:24.04 --vm\n\nlocal\n")
		return nil
	})
	got, err := l.defaultRemote(context.Background())
	require.NoError(t, err)
	assert.Equal(t, got, "local")
}

// an operation names its instance under resources (lxd 5) or, for a create
// on lxd 6, only in metadata.entity_url.
func TestLXDOperationNames(t *testing.T) {
	five := lxdOperation{Resources: map[string][]string{"instances": {"/1.0/instances/a?project=pats"}}}
	six := lxdOperation{Resources: map[string][]string{}, Metadata: map[string]any{"entity_url": "/1.0/instances/a?project=pats"}}
	for _, op := range []lxdOperation{five, six} {
		assert.That(t, op.names("/1.0/instances/a"), "%+v did not name a", op)
		assert.That(t, !op.names("/1.0/instances/ab"), "%+v named ab", op)
	}
}
