package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// fakeLXC records every lxc call and answers the few whose output matters.
type fakeLXC struct {
	calls   [][]string
	devices map[string]map[string]string // the expanded devices a query returns
	config  map[string]string
	failOn  string
}

func (f *fakeLXC) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
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

func (f *fakeLXC) call(prefix string) []string {
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return c
		}
	}
	return nil
}

func fakeLXD(o Options) (*lxd, *fakeLXC) {
	f := &fakeLXC{}
	l := newLXD(o)
	l.run = f.run
	return l, f
}

// an open sandbox: made stopped from the image in the pats project on the
// pinned remote, stamped as this run's, its own bridge and acl in the default
// project, the expanded config checked before start, the workdir pushed.
func TestLXDStartOpen(t *testing.T) {
	l, f := fakeLXD(Options{Driver: "lxd", Image: "pats-img-x", Limits: Limits{CPU: 2, Memory: "4GiB", Disk: "10GiB"}})
	work := t.TempDir()
	sess, err := l.Start(context.Background(), Spec{Workdir: work})
	require.NoError(t, err)
	s := sess.(*lxdSession)
	f.devices = map[string]map[string]string{}

	init := strings.Join(f.call("init"), " ")
	for _, want := range []string{"init pats-img-x srv:" + s.name, "--project pats", "--storage default",
		"user.pats.run=" + lxdRunToken, "limits.cpu=2", "limits.memory=4GiB", "root,size=10GiB"} {
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
	for i, c := range f.calls {
		switch c[0] {
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
	l, f := fakeLXD(Options{Driver: "lxd", Image: "img", Profiles: []string{"extra"}})
	f.devices = map[string]map[string]string{"eth9": {"type": "nic", "network": "lxdbr0"}}
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "would bypass the sandbox's egress")
	assert.Nil(t, f.call("start"))
	assert.Nil(t, f.call("network create"))
	assert.NotNil(t, f.call("delete"))
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "-p extra")

	l, f = fakeLXD(Options{Driver: "lxd", Image: "img"})
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

	l, f := fakeLXD(Options{Driver: "lxd", Image: "img", VM: true})
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "--vm -c agent.nic_config=true")
}

// HandOver writes the sudoers rule only when the pair allows sudo.
func TestLXDHandOverSudo(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		l, f := fakeLXD(Options{Driver: "lxd", Image: "img"})
		sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}, Sudo: sudo})
		require.NoError(t, err)
		f.calls = nil
		require.NoError(t, sess.HandOver(context.Background(), WorkMount))
		joined := ""
		for _, c := range f.calls {
			joined += strings.Join(c, " ") + "\n"
		}
		assert.ContainsString(t, joined, "chown -R 1001:1001")
		assert.Equal(t, strings.Contains(joined, "NOPASSWD"), sudo)
		assert.Equal(t, strings.Contains(joined, "rm -f /etc/sudoers.d/pats"), !sudo)
	}
}

// Sync replaces each writable bind's host copy with what is in the instance.
func TestLXDSync(t *testing.T) {
	l, _ := fakeLXD(Options{Driver: "lxd", Image: "img"})
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

func TestLXDName(t *testing.T) {
	assert.Equal(t, lxdName("/x/pats-work-123"), "pats-work-123")
	assert.Equal(t, lxdName("/x/Odd_Name.1"), "pats-odd-name-1")
	long := lxdName("/x/" + strings.Repeat("a", 80))
	assert.That(t, len(long) <= 63, "name too long: %d", len(long))
	assert.That(t, len(bridgeName("pats-work-123")) <= 15, "bridge name too long")
}

// a proxied sandbox: an addressless bridge with a reject acl, the agent on it
// at a static pool address, the canary before the proxy, the proxy container
// on the bridge and the uplink with forwarding off, listening on the bridge
// address only, and the agent told where it is.
func TestLXDStartProxy(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	t.Setenv(lxdProxyBinEnv, bin)
	t.Setenv(lxdUplinkEnv, "up0")
	l, f := fakeLXD(Options{Driver: "lxd", Image: "img"})
	answers := map[string]string{
		"egress-canary":   "egress-canary leak=0\n",
		"egress-reach":    "egress-reach ok=1\n",
		"id -u patsproxy": "999\n",
		"image info":      "Fingerprint: abc\n",
		"query srv:/1.0 ": `{"environment":{"architectures":["x86_64"]}}`,
	}
	inner := f.run
	l.run = func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ") + " "
		for k, v := range answers {
			if strings.Contains(joined, k) {
				f.calls = append(f.calls, args)
				io.WriteString(stdout, v)
				return nil
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	}
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, SelfTest: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	require.NoError(t, err)
	s := sess.(*lxdSession)

	net := strings.Join(f.call("network create"), " ")
	for _, want := range []string{"ipv4.address=none", "ipv6.address=none", "security.acls.default.egress.action=reject", "security.acls.default.ingress.action=reject"} {
		assert.ContainsString(t, net, want)
	}
	assert.Nil(t, f.call("network acl rule add")) // no rule: both defaults reject
	agentIP := s.egress.agentIP.String()
	proxyIP := s.egress.ip.String()
	assert.That(t, pool.Contains(s.egress.ip), "proxy %s outside the pool", proxyIP)
	all := ""
	for _, c := range f.calls {
		all += strings.Join(c, " ") + "\n"
	}
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
	all = ""
	for _, c := range f.calls {
		all += strings.Join(c, " ") + "\n"
	}
	assert.ContainsString(t, all, "delete srv:"+s.name+"-p")
	assert.ContainsString(t, all, "network delete srv:"+s.bridge)
}
