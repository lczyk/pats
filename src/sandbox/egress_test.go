package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// fakeDocker answers the docker cli the way the egress plumbing needs,
// recording every call. the `docker logs -f` follower prints the proxy's
// listening line and then runs until the proxy container is removed, the
// way the real follower sees EOF once the container is gone; graceful says
// it ended that way rather than killed.
type fakeDocker struct {
	callLog
	t        *testing.T
	removed  chan struct{} // closed by the first `rm`
	rmOnce   sync.Once
	graceful atomic.Bool
	failOn   string // substring of the joined args; matching call errors
	failOut  string // what that call prints
	failN    int    // how many matching calls fail (0: every one)
	failed   int
	out      []byte // what `--entrypoint cat` prints (the image trust bundle)
	canary   string // what the canary run prints
	reach    string // what the reachability probe prints; "" is a pass
}

func newFakeDocker(t *testing.T) *fakeDocker {
	return &fakeDocker{t: t, removed: make(chan struct{})}
}

func (f *fakeDocker) container() *container {
	return newContainer(testProcess(f.t), runFunc(f.run), Options{Driver: "docker"}).on(SandboxOptions{Image: "img"})
}

func (f *fakeDocker) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.record(args)
	joined := strings.Join(args, " ")
	if len(args) > 0 && args[0] == "rm" {
		f.rmOnce.Do(func() { close(f.removed) })
	}
	if len(args) > 1 && args[0] == "logs" && args[1] == "-f" {
		io.WriteString(stderr, "listening [::]:8080\n")
		select {
		case <-f.removed:
			f.graceful.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	fail := f.failOn != "" && strings.Contains(joined, f.failOn) && (f.failN == 0 || f.failed < f.failN)
	if fail {
		f.failed++
	}
	f.mu.Unlock()
	if fail {
		io.WriteString(stdout, f.failOut)
		return errors.New("fake docker failure")
	}
	switch {
	case strings.Contains(joined, "egress-reach"):
		if f.reach == "" {
			io.WriteString(stdout, "egress-reach ok=1")
		} else {
			io.WriteString(stdout, f.reach)
		}
	case strings.Contains(joined, "--entrypoint bash"):
		io.WriteString(stdout, f.canary)
	case strings.Contains(joined, "--entrypoint cat"):
		stdout.Write(f.out)
	}
	return nil
}

func TestSetupEgressModes(t *testing.T) {
	c := &container{driver: "docker", image: "img"}
	args, env, td, err := c.setupEgress(context.Background(), Spec{})
	require.NoError(t, err)
	assert.Nil(t, args)
	assert.Nil(t, env)
	assert.Nil(t, td)
	args, _, _, err = c.setupEgress(context.Background(), Spec{Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.EqualArrays(t, args, []string{"--network", "none"})
}

func TestStartEgressProxy(t *testing.T) {
	fd := newFakeDocker(t)
	c := fd.container()
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-x"),
		Egress: Egress{
			Set:    true,
			Policy: proxy.Policy{Allow: []string{"api.example.com"}, Deny: []string{"evil.example.com"}, DenyResponse: "404"},
		},
	}
	netArgs, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)

	// the pair's network is a /29 out of the pool and the proxy sits at its
	// second address; the agent joins it with the proxy url by ip and no
	// upstream resolver.
	args := fd.find("network create")
	assert.That(t, slices.Contains(args, "--internal"))
	assert.Equal(t, args[len(args)-1], "sbx-egr-pats-work-x")
	i := slices.Index(args, "--subnet")
	require.That(t, i > 0)
	subnet := netip.MustParsePrefix(args[i+1])
	assert.That(t, testPool.Contains(subnet.Addr()) && subnet.Bits() == 29, "subnet %s", subnet)
	proxyIP := subnet.Addr().Next().Next().String()

	joined := strings.Join(netArgs, " ")
	for _, want := range []string{
		"--network sbx-egr-pats-work-x",
		"--dns 0.0.0.0",
		"HTTP_PROXY=http://" + proxyIP + ":8080",
		"https_proxy=http://" + proxyIP + ":8080",
	} {
		assert.ContainsString(t, joined, want)
	}

	// the proxy runs with the policy as yaml over env, pinned to its ip.
	run := strings.Join(fd.find("run -d"), " ")
	for _, want := range []string{
		"--ip " + proxyIP,
		"--network sbx-egr-pats-work-x",
		"PATS_EGRESS_POLICY=",
		"allow:\n    - api.example.com",
		"deny:\n    - evil.example.com",
		"deny-response: \"404\"",
		"PATS_EGRESS_REFUSE=10.213.0.0/16",
		// reachable only from the pair's own network, routing nothing.
		"PATS_EGRESS_LISTEN=" + proxyIP + ":8080",
		"--sysctl net.ipv4.ip_forward=0",
		"--cap-drop ALL",
	} {
		assert.ContainsString(t, run, want)
	}
	assert.Equal(t, strings.Contains(run, "PATS_EGRESS_CA_KEY"), false) // no url entry: no mitm, no CA
	assert.NotNil(t, fd.find("network connect bridge"))

	teardown()
	assert.NotNil(t, fd.find("rm -f sbx-proxy-pats-work-x"))
	assert.NotNil(t, fd.find("network rm sbx-egr-pats-work-x"))
}

// COVER: the mitm security invariants -- CA key reaches the proxy over env
// only (never disk), the agent gets cert + merged bundle but not the key, and
// the proxy verifies upstream against the same bundle.
func TestStartEgressProxyMitm(t *testing.T) {
	fd := newFakeDocker(t)
	fd.out = []byte("-----BEGIN CERTIFICATE-----\nsystemroots\n-----END CERTIFICATE-----")
	c := fd.container()
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-m"),
		Egress: Egress{
			Set:    true,
			Policy: proxy.Policy{Deny: []string{"github.com/*/secrets*"}},
		},
	}
	netArgs, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)
	defer teardown()

	run := strings.Join(fd.find("run -d"), " ")
	for _, want := range []string{
		"PATS_EGRESS_POLICY=",
		"PATS_EGRESS_CA_CERT=-----BEGIN",
		"PATS_EGRESS_CA_KEY=-----BEGIN",
		"PATS_EGRESS_ROOTS=" + agentBundlePath,
		agentBundlePath + ":ro",
	} {
		assert.ContainsString(t, run, want)
	}

	// agent side: bundle + cert mounts and the tls env vars, no key anywhere.
	joined := strings.Join(netArgs, " ")
	for _, want := range []string{agentBundlePath + ":ro", agentCertPath + ":ro", "SSL_CERT_FILE=", "CARGO_HTTP_CAINFO=", "NODE_EXTRA_CA_CERTS=" + agentCertPath} {
		assert.ContainsString(t, joined, want)
	}
	assert.Equal(t, strings.Contains(joined, "PRIVATE KEY"), false)

	// the CA dir on disk holds cert + merged bundle only -- never the key.
	var bundlePath string
	for _, a := range netArgs {
		if strings.HasSuffix(a, agentBundlePath+":ro") {
			bundlePath = strings.TrimSuffix(a, ":"+agentBundlePath+":ro")
		}
	}
	require.NotEqual(t, bundlePath, "")
	caDir := filepath.Dir(bundlePath)
	ents, err := os.ReadDir(caDir)
	require.NoError(t, err)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(caDir, e.Name()))
		assert.Equal(t, strings.Contains(string(b), "PRIVATE KEY"), false)
	}
	// merged bundle keeps the image's system roots (tunneled hosts need them).
	b, _ := os.ReadFile(bundlePath)
	assert.ContainsString(t, string(b), "systemroots")
}

// COVER: the teardown order -- the proxy container is removed (which makes
// `docker logs -f` reach EOF) before the follower is waited on or killed. if
// teardown ever waited on or killed the follower first, it would only end
// by the grace-timeout kill, not on its own.
func TestStartEgressProxyRemovesProxyBeforeFollowerTeardown(t *testing.T) {
	fd := newFakeDocker(t)
	c := fd.container()
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-order"),
		Egress:  Egress{Set: true, AuditPath: filepath.Join(t.TempDir(), "audit.log")},
	}
	_, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() { teardown(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("teardown did not return within the grace window")
	}
	assert.That(t, fd.graceful.Load(), "the follower was killed, not ended by the proxy's removal")
}

func TestStartEgressProxyFailureTearsDown(t *testing.T) {
	fd := newFakeDocker(t)
	fd.failOn = "run -d"
	c := fd.container()
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-f"), Egress: Egress{Set: true}}
	_, _, err := c.startEgressProxy(context.Background(), spec)
	assert.Error(t, err, assert.AnyError)
	assert.NotNil(t, fd.find("network rm sbx-egr-pats-work-f"))
}

// a subnet the daemon already has (another pats, a leftover) is skipped for
// the next one; any other create failure is final, and removes the network
// in case the daemon made it after all.
func TestCreateNetworkMovesOnWhenTaken(t *testing.T) {
	fd := newFakeDocker(t)
	fd.failOn, fd.failOut, fd.failN = "network create", "Error: Pool overlaps with other one on this address space", 2
	c := fd.container()
	subnet, err := c.createNetwork(context.Background(), "sbx-egr-t", testPool)
	require.NoError(t, err)
	creates := 0
	for _, call := range fd.calls {
		if call[0] == "network" && call[1] == "create" {
			creates++
		}
	}
	assert.Equal(t, creates, 3)
	assert.Equal(t, subnet.Bits(), 29)

	fd = newFakeDocker(t)
	fd.failOn, fd.failOut = "network create", "Cannot connect to the Docker daemon"
	_, err = fd.container().createNetwork(context.Background(), "sbx-egr-t", testPool)
	assert.Error(t, err, "create network")
	assert.Equal(t, len(fd.calls), 2)
	assert.Equal(t, strings.Join(fd.calls[1], " "), "network rm sbx-egr-t")

	// a name taken is not a range taken: no other subnet helps, and the
	// network is not this pair's to remove.
	for _, msg := range []string{"Error response from daemon: network with name sbx-egr-t already exists",
		"Error: network name sbx-egr-t already used: network already exists"} {
		fd = newFakeDocker(t)
		fd.failOn, fd.failOut = "network create", msg
		_, err = fd.container().createNetwork(context.Background(), "sbx-egr-t", testPool)
		assert.Error(t, err, "the name sbx-egr-t is taken")
		assert.Equal(t, len(fd.calls), 1)
	}
	for _, msg := range []string{"Error: subnet 10.213.0.0/29 is already used on the host or by another config",
		"Error response from daemon: Pool overlaps with other one on this address space"} {
		assert.That(t, subnetTaken(msg) && !nameTaken(msg), "a subnet clash read as a name clash: %s", msg)
	}
}

func TestParsePool(t *testing.T) {
	p, err := parsePool("")
	require.NoError(t, err)
	assert.Equal(t, p.String(), "10.213.0.0/16")
	p, err = parsePool("172.30.4.0/22")
	require.NoError(t, err)
	assert.Equal(t, p.String(), "172.30.4.0/22")
	for _, bad := range []string{"::/0", "fd00::/48", "10.0.0.0/28", "nope"} {
		_, err := parsePool(bad)
		assert.Error(t, err, "PATS_EGRESS_POOL", bad)
	}
}

// an audit log that cannot be created fails the setup: running on without it
// would drop the cheat detector silently.
func TestStartEgressProxyAuditCreateFails(t *testing.T) {
	fd := newFakeDocker(t)
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-a"),
		Egress:  Egress{Set: true, AuditPath: filepath.Join(t.TempDir(), "missing", "dir", "egress.log")},
	}
	_, _, err := fd.container().startEgressProxy(context.Background(), spec)
	assert.Error(t, err, "audit log")
	assert.NotNil(t, fd.find("network rm"))
}

// what a driver makes is its process's: under that process's temp root and
// owned by its run, so two processes stay two runs to pats clean.
func TestPrepareUsesItsProcess(t *testing.T) {
	p := testProcess(t)
	home, err := p.home()
	require.NoError(t, err)
	eg := Egress{Set: true, Policy: proxy.Policy{Deny: []string{"a.test/x*"}}}
	m, err := p.prepare(eg, nil, func() ([]byte, error) { return []byte("roots"), nil })
	require.NoError(t, err)
	defer m.cleanup()
	assert.Equal(t, filepath.Dir(m.caDir), filepath.Join(home, "tmp"))
	owner, err := os.ReadFile(m.caDir + ownerSuffix)
	require.NoError(t, err)
	assert.Equal(t, string(owner), p.token)
}

// a failed prepare removes the CA dir it already made.
func TestPrepareCleansUpOnError(t *testing.T) {
	p := testProcess(t)
	cache, err := p.home()
	require.NoError(t, err)
	eg := Egress{Set: true, Policy: proxy.Policy{Deny: []string{"a.test/x*"}}}
	_, err = p.prepare(eg, nil, func() ([]byte, error) { return nil, errors.New("no bundle") })
	assert.Error(t, err, "trust bundle")
	var left []string
	_ = filepath.WalkDir(cache, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && strings.Contains(d.Name(), "mitm-ca-") {
			left = append(left, p)
		}
		return nil
	})
	assert.Equal(t, len(left), 0)
}

// testPool is the default egress pool.
var testPool = netip.MustParsePrefix("10.213.0.0/16")

func TestNextSubnet(t *testing.T) {
	p := newProcess(wallClock{}, defaultTimings)
	a, b := p.nextSubnet(testPool), p.nextSubnet(testPool)
	assert.That(t, testPool.Contains(a.Addr()) && testPool.Contains(b.Addr()))
	assert.Equal(t, a.Bits(), 29)
	assert.NotEqual(t, a, b)
	assert.That(t, !a.Contains(b.Addr()), "consecutive slots overlap: %s %s", a, b)
}

// podman's default network is `podman`, and its resolver forwards nothing on
// an internal network, so the agent gets no dns override there.
func TestPodmanDefaults(t *testing.T) {
	fd := newFakeDocker(t)
	c := fd.container()
	c.driver = "podman"
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-p"), Egress: Egress{Set: true}}
	netArgs, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)
	defer teardown()
	assert.NotNil(t, fd.find("network connect podman"))
	assert.ContainsString(t, strings.Join(fd.find("network create"), " "), "--disable-dns")
	assert.Equal(t, strings.Contains(strings.Join(netArgs, " "), "--dns"), false)
	assert.Equal(t, (&container{driver: "docker"}).defaultNetwork(), "bridge")
	assert.Equal(t, strings.Contains(strings.Join(fd.find("network create"), " "), "inhibit_ipv4"), false)
}

// docker puts a host address on an internal network's bridge unless told not
// to, and every host service listening on it would be in the agent's reach.
func TestDockerNetworkHidesHost(t *testing.T) {
	fd := newFakeDocker(t)
	c := fd.container()
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-d"), Egress: Egress{Set: true}}
	_, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)
	defer teardown()
	assert.ContainsString(t, strings.Join(fd.find("network create"), " "), "com.docker.network.bridge.inhibit_ipv4=true")
}

func TestEgressCanary(t *testing.T) {
	fd := newFakeDocker(t)
	fd.canary = "egress-canary leak=0"
	require.NoError(t, fd.container().egressCanary(context.Background(), "n"))
	fd.canary = "egress-canary leak=1"
	assert.Error(t, fd.container().egressCanary(context.Background(), "n"), "not fail-closed")
	fd.canary = "bash: not found"
	assert.Error(t, fd.container().egressCanary(context.Background(), "n"), "no verdict")
	fd.canary = "egress-canary missing=getent"
	assert.Error(t, fd.container().egressCanary(context.Background(), "n"), "could not run")

	// the self-test runs before the proxy and refuses the run on a leak.
	fd = newFakeDocker(t)
	fd.canary = "egress-canary leak=1"
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-c"), Egress: Egress{Set: true, SelfTest: true}}
	_, _, err := fd.container().startEgressProxy(context.Background(), spec)
	assert.Error(t, err, "not fail-closed")
	assert.Nil(t, fd.find("run -d"))
	assert.NotNil(t, fd.find("network rm"))

	// with the proxy up, its port must answer from the internal network.
	fd = newFakeDocker(t)
	fd.canary = "egress-canary leak=0"
	fd.reach = "egress-reach ok=0"
	spec = Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-r"), Egress: Egress{Set: true, SelfTest: true}}
	_, _, err = fd.container().startEgressProxy(context.Background(), spec)
	assert.Error(t, err, "cannot reach the proxy")
	assert.NotNil(t, fd.find("rm -f sbx-proxy-pats-work-r"))
	fd.reach = ""
	spec.Workdir = filepath.Join(t.TempDir(), "pats-work-s")
	_, teardown, err := fd.container().startEgressProxy(context.Background(), spec)
	require.NoError(t, err)
	teardown()
}

func TestWaitReady(t *testing.T) {
	assert.NoError(t, waitReady(strings.NewReader("warming up\nlistening [::]:8080\nmore\n"), wallClock{}, time.Second))
	assert.Error(t, waitReady(strings.NewReader("Error: No such container\n"), wallClock{}, time.Second), "exited before listening: Error: No such container")
	// the last line said, not the empty read after it.
	assert.Error(t, waitReady(strings.NewReader("error: upstream http://p:1 unreachable\n\n"), wallClock{}, time.Second), "exited before listening: error: upstream http://p:1 unreachable")
	r, w := io.Pipe()
	defer w.Close()
	assert.Error(t, waitReady(r, wallClock{}, 50*time.Millisecond), "not listening")
}

func TestProxyEnvOptions(t *testing.T) {
	env := map[string]string{
		"PATS_EGRESS_POLICY":  "default: allow\ndeny: [evil.test]\n",
		"PATS_EGRESS_CA_CERT": "-----BEGIN CERTIFICATE-----",
		"PATS_EGRESS_REFUSE":  "10.213.0.0/16, 10.0.0.0/8",
		"PATS_EGRESS_ROOTS":   "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----",
	}
	o, err := ProxyEnvOptions(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, o.Policy.Default, "allow")
	assert.EqualArrays(t, o.Policy.Deny, []string{"evil.test"})
	assert.Equal(t, len(o.Refuse), 2)
	assert.ContainsString(t, string(o.Roots), "BEGIN CERTIFICATE")
	assert.Equal(t, o.Chain == nil, true)

	env["PATS_EGRESS_UPSTREAM_HTTPS"] = "http://proxy.corp:3128"
	o, err = ProxyEnvOptions(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, o.Chain.HTTPS.Host, "proxy.corp:3128")

	env["PATS_EGRESS_POLICY"] = "default: [x"
	_, err = ProxyEnvOptions(func(k string) string { return env[k] })
	assert.Error(t, err, "PATS_EGRESS_POLICY")
}

// an agent is named after its workdir only when MkTemp made it: elsewhere the
// base name need not be unique (t.TempDir's 001) and clean could not judge it.
func TestAgentName(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	work, err := MkTemp("pats-work-")
	require.NoError(t, err)
	defer RemoveTemp(work)
	assert.Equal(t, agentName(work), "sbx-agent-"+filepath.Base(work))
	assert.Equal(t, agentName(t.TempDir()), "")
}

// the client id is published whole: concurrent first runs agree on one, and a
// malformed file is replaced rather than poisoning every later run.
func TestLoadClientID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "client-id")
	ids := make(chan string, 8)
	for i := range 8 {
		go func(i int) {
			id, err := loadClientID(p, func() string { return fmt.Sprintf("%016x", i+1) })
			if err != nil {
				id = "err: " + err.Error()
			}
			ids <- id
		}(i)
	}
	first := <-ids
	for range 7 {
		assert.Equal(t, <-ids, first)
	}
	require.NoError(t, os.WriteFile(p, []byte("half"), 0o600))
	id, err := loadClientID(p, func() string { return "00000000000000ff" })
	require.NoError(t, err)
	assert.Equal(t, id, "00000000000000ff")
}

// every engine says what its networks need; the table is the one place.
func TestEngineTable(t *testing.T) {
	assert.EqualArrays(t, Engines(), []string{"docker", "podman"})
	for _, name := range Engines() {
		assert.That(t, engines[name].defaultNetwork != "", "%s: no default network", name)
	}
	assert.That(t, IsEngine("docker") && IsEngine("podman") && !IsEngine("lxd"), "IsEngine")
}

// a bundle is read once per key, a failed read is not kept, and a caller
// waiting on another's read leaves when its own context ends.
func TestBundleCache(t *testing.T) {
	var c bundleCache
	reads := 0
	read := func() ([]byte, error) { reads++; return []byte("pem"), nil }
	_, err := c.get(context.Background(), "a", func() ([]byte, error) { return nil, errors.New("down") })
	assert.Error(t, err, "down")
	for range 3 {
		got, err := c.get(context.Background(), "a", read)
		require.NoError(t, err)
		assert.Equal(t, string(got), "pem")
	}
	assert.Equal(t, reads, 1)
	_, err = c.get(context.Background(), "b", read)
	require.NoError(t, err)
	assert.Equal(t, reads, 2)

	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = c.get(context.Background(), "slow", func() ([]byte, error) { close(started); <-release; return nil, nil })
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.get(ctx, "slow", read)
	assert.ErrorIs(t, err, context.Canceled)
	close(release)
}
