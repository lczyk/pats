package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// machine is a machine proxy at upstream (a url), with trust when given.
func machine(t *testing.T, upstream string, trust []byte) *MachineProxy {
	t.Helper()
	c, err := proxy.NewChain(upstream, upstream, ".corp.test")
	require.NoError(t, err)
	return &MachineProxy{Chain: c, Trust: trust}
}

// execEnv is the env of the running exec of argv0 in inst, from a fake's
// execs; the caller holds no lock.
func execEnv(execs func() map[*fakeExec]bool, inst string) map[string]string {
	for x := range execs() {
		if x.inst == inst {
			return x.env
		}
	}
	return nil
}

func TestReadMachineProxy(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	m, err := ReadMachineProxy(env(map[string]string{"ALL_PROXY": "http://p:1"}))
	require.NoError(t, err)
	assert.Equal(t, m == nil, true)

	_, err = ReadMachineProxy(env(map[string]string{"HTTPS_PROXY": "socks5://p:1"}))
	assert.Error(t, err, "socks5")

	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	require.NoError(t, os.WriteFile(junk, []byte("nope"), 0o644))
	_, err = ReadMachineProxy(env(map[string]string{"HTTPS_PROXY": "http://p:1", "SSL_CERT_FILE": junk}))
	assert.Error(t, err, "no certificates")

	cert, _, err := GenCA()
	require.NoError(t, err)
	ca := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(ca, cert, 0o644))
	m, err = ReadMachineProxy(env(map[string]string{"https_proxy": "http://p:1", "SSL_CERT_FILE": ca}))
	require.NoError(t, err)
	assert.Equal(t, string(m.Trust), string(cert))
	// SSL_CERT_FILE alone is not a proxy.
	m, err = ReadMachineProxy(env(map[string]string{"SSL_CERT_FILE": ca}))
	require.NoError(t, err)
	assert.Equal(t, m == nil, true)
}

// a credential in the upstream rides in the cli's environment, never on its
// command line.
func TestMachineProxySidecarEnvOffArgv(t *testing.T) {
	m := machine(t, "http://u:secret@proxy.corp:3128", nil)
	flags, r := envFlags(cliRunner("docker"), "-e", m.Chain.Env())
	assert.That(t, !strings.Contains(strings.Join(flags, " "), "secret"), "credential on argv: %v", flags)
	assert.ContainsString(t, strings.Join(r.(execRunner).env, " "), "PATS_EGRESS_UPSTREAM_HTTPS=http://u:secret@proxy.corp:3128")
}

func TestMachineProxyStartRefusals(t *testing.T) {
	f := newFakeDaemon(t, "docker")
	o := Options{Driver: "docker", Machine: machine(t, "http://127.0.0.1:3128", nil)}
	c := newContainer(testProcess(t), runFunc(f.run), o).on(SandboxOptions{Image: "img"})
	_, err := c.Start(context.Background(), Spec{Workdir: workdir(t)})
	assert.Error(t, err, "env -u HTTPS_PROXY -u https_proxy -u HTTP_PROXY -u http_proxy pats")
	assert.Error(t, err, "on this host's loopback, which a docker sandbox cannot reach")
	sess, err := c.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{None: true}})
	require.NoError(t, err)
	require.NoError(t, sess.Close())

	o = Options{Driver: "docker", machineErr: errors.New("HTTPS_PROXY: the upstream is a socks5:// proxy")}
	c = newContainer(testProcess(t), runFunc(f.run), o).on(SandboxOptions{Image: "img"})
	_, err = c.Start(context.Background(), Spec{Workdir: workdir(t)})
	assert.Error(t, err, "socks5")

	fs := newFakeServer(t)
	l := lxdOn(t, fs, Options{Machine: machine(t, "http://localhost:3128", nil)})
	_, err = l.Start(context.Background(), Spec{Workdir: workdir(t)})
	assert.Error(t, err, "a lxd sandbox cannot reach")
}

func TestMachineProxyContainer(t *testing.T) {
	cert, _, err := GenCA()
	require.NoError(t, err)
	for _, driver := range Engines() {
		t.Run(driver, func(t *testing.T) {
			f := newFakeDaemon(t, driver)
			f.upstream = "proxy.corp:3128"
			o := Options{Driver: driver, Machine: machine(t, "http://u:secret@proxy.corp:3128", cert)}
			c := newContainer(testProcess(t), runFunc(f.run), o).on(SandboxOptions{Image: "img"})

			// open: the machine's proxy env on every exec, and its trust.
			var env map[string]string
			f.agent = func(ctx context.Context, ctr string, argv []string, stdout, stderr io.Writer) error {
				f.mu.Lock()
				env = execEnv(func() map[*fakeExec]bool { return f.execs }, ctr)
				f.mu.Unlock()
				return nil
			}
			sess, err := c.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{SelfTest: true}})
			require.NoError(t, err)
			s := sess.(*containerSession)
			f.mu.Lock()
			for _, k := range proxyVars {
				v, set := f.ctrs[s.id].env[k]
				assert.That(t, set && v == "", "open container %s=%q, want set empty", k, v)
			}
			f.mu.Unlock()
			_, err = sess.Exec(context.Background(), Cmd{Argv: []string{"agent"}}, io.Discard, io.Discard)
			require.NoError(t, err)
			assert.Equal(t, env["HTTPS_PROXY"], "http://u:secret@proxy.corp:3128")
			assert.Equal(t, env["https_proxy"], "http://u:secret@proxy.corp:3128")
			assert.Equal(t, env["NO_PROXY"], ".corp.test")
			assert.Equal(t, env["SSL_CERT_FILE"], agentBundlePath)
			bundle, ok := f.read(s.id, agentBundlePath)
			assert.That(t, ok, "no trust bundle in an open sandbox")
			assert.ContainsString(t, bundle, string(cert))
			require.NoError(t, sess.Close())

			// a policy: the sidecar gets the upstream under pats's keys, the
			// agent pats's proxy and empty NO_PROXY / ALL_PROXY.
			f.calls = nil
			sess, err = c.Start(context.Background(), Spec{Workdir: workdir(t),
				Egress: Egress{Set: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
			require.NoError(t, err)
			s = sess.(*containerSession)
			f.mu.Lock()
			var side, agent *fakeCtr
			for _, ctr := range f.ctrs {
				if strings.HasPrefix(ctr.name, namePrefix+"proxy-") {
					side = ctr
				}
				if ctr.id == s.id {
					agent = ctr
				}
			}
			f.mu.Unlock()
			require.NotNil(t, side)
			assert.Equal(t, side.env[proxy.EnvUpstreamHTTPS], "http://u:secret@proxy.corp:3128")
			assert.Equal(t, side.env[proxy.EnvNoProxy], ".corp.test")
			for _, k := range proxyVars {
				v, set := side.env[k]
				assert.That(t, set && v == "", "sidecar %s=%q, want set empty", k, v)
			}
			assert.ContainsString(t, agent.env["HTTPS_PROXY"], ":"+proxyPort)
			assert.Equal(t, agent.env["NO_PROXY"], "")
			assert.Equal(t, agent.env["ALL_PROXY"], "")
			assert.Equal(t, agent.env["SSL_CERT_FILE"], agentBundlePath)
			run := f.call("run -d --rm --init")
			assert.Equal(t, strings.Contains(strings.Join(run, " "), "--http-proxy=false"), driver == "podman")
			require.NoError(t, sess.Close())

			// an upstream nobody reaches fails the start, by name, the
			// credential redacted: the open self-test, and the sidecar.
			f.upstream = ""
			_, err = c.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{SelfTest: true}})
			assert.Error(t, err, "the upstream http://u:xxxxx@proxy.corp:3128: unreachable from the")
			f.upstreamDown = true
			_, err = c.Start(context.Background(), Spec{Workdir: workdir(t),
				Egress: Egress{Set: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
			assert.Error(t, err, "network, behind the upstream http://u:xxxxx@proxy.corp:3128")
			assert.Error(t, err, "exited before listening: error: upstream")
		})
	}
}

func TestMachineProxyLXD(t *testing.T) {
	cert, _, err := GenCA()
	require.NoError(t, err)
	f := newFakeServer(t)
	f.upstream = "10.20.30.40:3128"
	l := lxdOn(t, f, Options{ProxyBin: proxyBin(t), Machine: machine(t, "http://u:secret@10.20.30.40:3128", cert)})

	// open: the probe reaches the upstream, which the acl lets through, and
	// the agent gets the machine's proxy env, never on a command line.
	var env map[string]string
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		f.mu.Lock()
		env = execEnv(func() map[*fakeExec]bool { return f.execs }, inst)
		f.mu.Unlock()
		return nil
	}
	sess, err := l.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{SelfTest: true}})
	require.NoError(t, err)
	require.NoError(t, sess.HandOver(context.Background(), WorkMount))
	_, err = sess.Exec(context.Background(), Cmd{Argv: []string{"agent"}}, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, env["HTTPS_PROXY"], "http://u:secret@10.20.30.40:3128")
	assert.Equal(t, env["SSL_CERT_FILE"], agentBundlePath)
	require.NoError(t, sess.Sync(context.Background()))
	require.NoError(t, sess.Close())
	for _, c := range f.all() {
		assert.That(t, !strings.Contains(c, "secret"), "credential on a command line: %s", c)
	}

	// a policy: the proxy instance gets the upstream.
	f.calls = nil
	var penv map[string]string
	f.before = func(args []string) {
		for x := range f.execs {
			if x.env[proxy.EnvUpstreamHTTPS] != "" {
				penv = x.env
			}
		}
	}
	sess, err = l.Start(context.Background(), Spec{Workdir: workdir(t),
		Egress: Egress{Set: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	require.NoError(t, err)
	require.NoError(t, sess.Close())
	assert.Equal(t, penv[proxy.EnvUpstreamHTTPS], "http://u:secret@10.20.30.40:3128")
	for _, c := range f.all() {
		assert.That(t, !strings.Contains(c, "secret"), "credential on a command line: %s", c)
	}

	// an upstream nobody reaches fails the start, by name: the open self-test,
	// and a policy sandbox's proxy.
	f.upstream = ""
	_, err = l.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{SelfTest: true}})
	assert.Error(t, err, "cannot reach the upstream http://u:xxxxx@10.20.30.40:3128")
	_, err = l.Start(context.Background(), Spec{Workdir: workdir(t),
		Egress: Egress{Set: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	assert.Error(t, err, "on the uplink, behind the upstream http://u:xxxxx@10.20.30.40:3128")
	assert.Error(t, err, "exited before listening: error: upstream")
}

func TestCarve(t *testing.T) {
	got := carve([]string{"10.0.0.0/8", "192.168.0.0/16"}, []netip.Addr{netip.MustParseAddr("10.20.30.40")})
	var in10 []netip.Prefix
	for _, s := range got {
		p := netip.MustParsePrefix(s)
		assert.That(t, !p.Contains(netip.MustParseAddr("10.20.30.40")), "%s still holds the upstream", s)
		if p.Overlaps(netip.MustParsePrefix("10.0.0.0/8")) {
			in10 = append(in10, p)
		}
	}
	assert.Equal(t, len(in10), 24) // one prefix per bit below /8
	assert.That(t, strings.Contains(strings.Join(got, ","), "192.168.0.0/16"), "untouched range lost: %v", got)
	for _, a := range []string{"10.0.0.1", "10.20.30.41", "10.255.255.255"} {
		covered := false
		for _, p := range in10 {
			covered = covered || p.Contains(netip.MustParseAddr(a))
		}
		assert.That(t, covered, "%s fell out of the rejects", a)
	}
}

// an upstream that inspects tls with a CA found only in SSL_CERT_FILE: an
// open agent, a hosts-only agent and pats's own tls inspection all get
// through it, verifying every certificate.
func TestMachineProxyTrust(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin:"+r.URL.Path)
	}))
	defer origin.Close()
	originPool := x509.NewCertPool()
	originPool.AddCert(origin.Certificate())

	// the machine's proxy: inspects every host with its own CA, which only
	// SSL_CERT_FILE holds, and reaches the origin for any name.
	corpCert, corpKey, err := GenCA()
	require.NoError(t, err)
	signer, err := proxy.NewSigner(string(corpCert), string(corpKey))
	require.NoError(t, err)
	rule, err := proxy.Policy{Default: "allow", Methods: []string{"GET"}}.Rule()
	require.NoError(t, err)
	toOrigin := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(origin.URL, "https://"))
	}
	corp := httptest.NewServer(&proxy.Proxy{Rule: rule, Signer: signer, Dial: toOrigin,
		Upstream: &http.Transport{DialContext: toOrigin, TLSClientConfig: &tls.Config{RootCAs: originPool}}})
	defer corp.Close()
	corpURL, _ := url.Parse(corp.URL)
	chain, err := proxy.NewChain(corp.URL, corp.URL, "")
	require.NoError(t, err)
	system := func() ([]byte, error) { return []byte("system roots\n"), nil }

	agentGet := func(t *testing.T, via *url.URL, bundlePath, target string) string {
		t.Helper()
		b, err := os.ReadFile(bundlePath)
		require.NoError(t, err)
		pool := x509.NewCertPool()
		require.That(t, pool.AppendCertsFromPEM(b), "no certificates in the bundle")
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(via),
			TLSClientConfig: &tls.Config{RootCAs: pool}}}
		resp, err := client.Get(target)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	patsProxy := func(t *testing.T, pol proxy.Policy) (*url.URL, *material) {
		t.Helper()
		m, err := testProcess(t).prepare(Egress{Set: true, Policy: pol}, corpCert, system)
		require.NoError(t, err)
		t.Cleanup(m.cleanup)
		penv := mergeEnv(m.proxyEnv(m.bundlePath), chain.Env())
		o, err := ProxyEnvOptions(func(k string) string { return penv[k] })
		require.NoError(t, err)
		p, err := proxy.New(o)
		require.NoError(t, err)
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		return u, m
	}

	t.Run("open", func(t *testing.T) {
		m, err := testProcess(t).prepare(Egress{}, corpCert, system)
		require.NoError(t, err)
		defer m.cleanup()
		assert.Equal(t, agentGet(t, corpURL, m.bundlePath, "https://example.com/open"), "origin:/open")
	})
	t.Run("hosts only", func(t *testing.T) {
		via, m := patsProxy(t, proxy.Policy{Allow: []string{"example.com"}})
		assert.Equal(t, agentGet(t, via, m.bundlePath, "https://example.com/tunnel"), "origin:/tunnel")
	})
	t.Run("tls inspection", func(t *testing.T) {
		via, m := patsProxy(t, proxy.Policy{Allow: []string{"example.com/x/*"}})
		assert.Equal(t, agentGet(t, via, m.bundlePath, "https://example.com/x/mitm"), "origin:/x/mitm")
	})
}
