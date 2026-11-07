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

func fakeLXD(t *testing.T, o Options) (*lxd, *fakeLXC) {
	f := &fakeLXC{}
	return newLXD(testProcess(t), runFunc(f.run), o), f
}

// an open sandbox: made stopped from the image (on its own server) in the pats project on the
// pinned remote, stamped as this run's, its own bridge and acl in the default
// project, the expanded config checked before start, the workdir pushed.
func TestLXDStartOpen(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Image: "pats-img-x", Limits: Limits{CPU: 2, Memory: "4GiB", Disk: "10GiB"}})
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
	l, f := fakeLXD(t, Options{Driver: "lxd", Image: "img", Profiles: []string{"extra"}})
	f.devices = map[string]map[string]string{"eth9": {"type": "nic", "network": "lxdbr0"}}
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "would bypass the sandbox's egress")
	assert.Nil(t, f.call("start"))
	assert.Nil(t, f.call("network create"))
	assert.NotNil(t, f.call("delete"))
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "-p extra")

	l, f = fakeLXD(t, Options{Driver: "lxd", Image: "img"})
	f.config = map[string]string{"raw.lxc": "lxc.net.0.type=veth"}
	_, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "raw config")
}

// the agent runs as the pats user, root steps as root, both with the egress
// env under their own. a vm carries agent.nic_config.
func TestLXDExecArgs(t *testing.T) {
	s := &lxdSession{l: &lxd{o: Options{Remote: "srv"}}, name: "pats-x", uid: "1001", env: map[string]string{"A": "1", "B": "2"}}
	args := strings.Join(s.execArgs(Cmd{Argv: []string{"run", "it"}, Env: map[string]string{"B": "3"}}), " ")
	assert.Equal(t, args, "exec srv:pats-x --project pats --force-noninteractive --user 1001 --group 1001 --cwd /workspace --env A=1 --env B=3 -- run it")
	args = strings.Join(s.execArgs(Cmd{Argv: []string{"id"}, Root: true, Dir: "/"}), " ")
	assert.Equal(t, args, "exec srv:pats-x --project pats --force-noninteractive --user 0 --group 0 --cwd / --env A=1 --env B=2 -- id")

	l, f := fakeLXD(t, Options{Driver: "lxd", Image: "img", VM: true})
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "--vm -c agent.nic_config=true")
}

// HandOver writes the sudoers rule only when the pair allows sudo.
func TestLXDHandOverSudo(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		l, f := fakeLXD(t, Options{Driver: "lxd", Image: "img"})
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
	l, _ := fakeLXD(t, Options{Driver: "lxd", Image: "img"})
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

// a tar entry may not leave the destination, and nothing is written through
// a symlink the stream itself made.
func TestReadTarSafety(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Close()
	assert.Error(t, readTar(&buf, t.TempDir()), "leaves the destination")

	outside := t.TempDir()
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: outside, Typeflag: tar.TypeSymlink})
	tw.WriteHeader(&tar.Header{Name: "link/pwned", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	dir := t.TempDir()
	_ = readTar(&buf, dir)
	_, err := os.Stat(filepath.Join(outside, "pwned"))
	assert.That(t, os.IsNotExist(err), "a file was written through a symlink")
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
	l, f := fakeLXD(t, Options{Driver: "lxd", Image: "img", ProxyBin: bin, Uplink: "up0"})
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
	assert.ContainsString(t, all, "--env PATS_EGRESS_LISTEN="+proxyIP+":8080")
	assert.ContainsString(t, all, "--user 999 --group 999 --cwd / --env PATS_EGRESS_LISTEN")
	assert.Equal(t, s.env["HTTP_PROXY"], "http://"+proxyIP+":8080")

	require.NoError(t, sess.Close())
	all = strings.Join(f.all(), "\n")
	assert.ContainsString(t, all, "delete srv:"+s.name+"-p")
	assert.ContainsString(t, all, "network delete srv:"+s.bridge)
}

// clean groups by run token: a run whose marker's lease is stale goes,
// instances before bridges before acls before the marker; a run with a fresh
// lease stays unless forced; a run with no marker goes once all of it is old;
// cached images stay always.
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
		},
		"/1.0/network-acls": []map[string]any{
			{"name": "patsbrdead", "config": cfg("dead", old)},
			marker("dead", old),
			marker("live", fresh),
		},
		"/1.0/images": []map[string]any{
			{"fingerprint": "aaaaaaaaaaaaaaaa", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-k"}}},
			{"fingerprint": "bbbbbbbbbbbbbbbb", "properties": cfg("dead", old), "aliases": []map[string]string{{"name": "pats-img-k-dead"}}},
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
	require.NoError(t, l.clean(context.Background(), false, false, &out))
	log := strings.Join(calls, "\n")
	for _, want := range []string{"delete srv:pats-dead", "network delete srv:patsbrdead", "network acl delete srv:patsbrdead",
		"image delete srv:bbbbbbbbbbbbbbbb", "network acl delete srv:pats-run-dead", "delete srv:pats-orphan"} {
		assert.ContainsString(t, log, want)
	}
	for _, keep := range []string{"srv:pats-live", "srv:pats-young", "srv:user-thing", "srv:lxdbr0", "aaaaaaaaaaaaaaaa"} {
		assert.Equal(t, strings.Contains(log, "delete "+keep), false, "%s removed", keep)
	}
	assert.That(t, strings.Index(log, "delete srv:pats-dead") < strings.Index(log, "network delete srv:patsbrdead"), "bridge before its instance")
	assert.That(t, strings.Index(log, "network acl delete srv:patsbrdead") < strings.Index(log, "network acl delete srv:pats-run-dead"), "marker before the rest")
	assert.ContainsString(t, out.String(), "left 2 live run(s)")

	calls = nil
	require.NoError(t, l.clean(context.Background(), true, false, &out))
	assert.ContainsString(t, strings.Join(calls, "\n"), "delete srv:pats-live")
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
		spec := LXDImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"},
			Progress: func(m string) { steps = append(steps, m) }, Warn: func(m string) { warns = append(warns, m) }}
		_, _, err := l.image(context.Background(), spec, io.Discard)
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

// the proxy image, built on the driver's own, reports its steps through the
// driver's Progress like a sandbox image build does.
func TestLXDProxyImageProgress(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	var steps []string
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv", ProxyBin: bin, Uplink: "up0", Progress: func(m string) { steps = append(steps, m) }})
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
	_, err := l.buildProxyImage(context.Background())
	require.NoError(t, err)
	require.That(t, len(steps) > 0, "no progress")
	assert.ContainsString(t, steps[0], "egress proxy: building lxd image pats-img-")
}
