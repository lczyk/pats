package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// fakeUpstream is the machine's proxy: CONNECT tunnels to origin whatever the
// target names, absolute-form requests it answers itself, and it records what
// it was sent. with auth set it answers anything without that basic auth 407.
type fakeUpstream struct {
	origin string // host:port every CONNECT reaches
	auth   string // user:pass, empty: no auth asked
	framed bool   // the CONNECT 200 carries a Content-Length, as some proxies send
	mu     sync.Mutex
	seen   []string // "METHOD target auth"
	srv    *httptest.Server
}

func newFakeUpstream(t *testing.T, origin, auth string) *fakeUpstream {
	f := &fakeUpstream{origin: origin, auth: auth}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) url(userinfo string) string {
	u := strings.TrimPrefix(f.srv.URL, "http://")
	if userinfo != "" {
		return "http://" + userinfo + "@" + u
	}
	return "http://" + u
}

func (f *fakeUpstream) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	target := r.RequestURI
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+target+" "+r.Header.Get("Proxy-Authorization"))
	f.mu.Unlock()
	if f.auth != "" && r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(f.auth)) {
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		io.WriteString(w, "via upstream:"+r.URL.String())
		return
	}
	remote, err := net.Dial("tcp", f.origin)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	client, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		remote.Close()
		return
	}
	if f.framed {
		client.Write([]byte("HTTP/1.1 200 Connection established\r\nContent-Length: 5\r\n\r\n"))
	} else {
		client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}
	go pipe(remote, client)
	go pipe(client, remote)
}

// chainedProxy serves o behind the chain, returning its url and audit.
func chainedProxy(t *testing.T, o Options) (*url.URL, *auditBuf) {
	t.Helper()
	var audit auditBuf
	o.Audit = &audit
	p, err := New(o)
	require.NoError(t, err)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, &audit
}

func mustChain(t *testing.T, https, http, noProxy string) *Chain {
	t.Helper()
	c, err := NewChain(https, http, noProxy)
	require.NoError(t, err)
	return c
}

// the origin is a tls test server, whose certificate names example.com: every
// name below is example.com, which the fake upstream resolves to the origin,
// so pats's proxy never resolves or guards a name itself.
func TestChainTunnelByConnect(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin:"+r.URL.Path)
	}))
	defer origin.Close()
	up := newFakeUpstream(t, strings.TrimPrefix(origin.URL, "https://"), "u:p@ss")
	purl, audit := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com"}},
		Chain:  mustChain(t, up.url("u:p%40ss"), "", ""),
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://example.com/x")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, string(body), "origin:/x")
	want := "CONNECT example.com:443 Basic " + base64.StdEncoding.EncodeToString([]byte("u:p@ss"))
	assert.EqualArrays(t, up.log(), []string{want})
	ev := audit.last(t)
	assert.Equal(t, ev.Allowed, true)
	assert.Equal(t, ev.Kind, "tunnel")
}

// a CONNECT 200 that carries framing headers still hands the tunnel over at
// once: nothing waits on a body.
func TestChainConnectFramed(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin")
	}))
	defer origin.Close()
	up := newFakeUpstream(t, strings.TrimPrefix(origin.URL, "https://"), "")
	up.framed = true
	purl, _ := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com"}},
		Chain:  mustChain(t, up.url(""), "", ""),
	})
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(purl),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://example.com/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, string(body), "origin")
}

func TestChainMitmThroughConnect(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin:"+r.URL.Path)
	}))
	defer origin.Close()
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	up := newFakeUpstream(t, strings.TrimPrefix(origin.URL, "https://"), "")
	certPath, keyPath, pool := testCA(t, t.TempDir())
	purl, audit := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com/ok*"}},
		CACert: certPath, CAKey: keyPath, Roots: roots,
		Chain: mustChain(t, up.url(""), "", ""),
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl),
		TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://example.com/ok")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, string(body), "origin:/ok")
	assert.EqualArrays(t, up.log(), []string{"CONNECT example.com:443 "})
	ev := audit.last(t)
	assert.Equal(t, ev.Kind, "mitm")
	assert.Equal(t, ev.Status, 200)
}

func TestChainPlainHTTPAbsoluteForm(t *testing.T) {
	up := newFakeUpstream(t, "", "u:p")
	purl, audit := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com"}},
		Chain:  mustChain(t, "", up.url("u:p"), ""),
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
	resp, err := client.Get("http://example.com/pkg?x=1")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, string(body), "via upstream:http://example.com/pkg?x=1")
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	assert.EqualArrays(t, up.log(), []string{"GET http://example.com/pkg?x=1 " + auth})
	assert.Equal(t, audit.last(t).Status, 200)
}

func TestChainNoProxyDirect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "direct")
	}))
	defer origin.Close()
	up := newFakeUpstream(t, "", "")
	host := strings.TrimPrefix(origin.URL, "http://")
	purl, _ := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{host}, AllowPrivate: true},
		Chain:  mustChain(t, up.url(""), up.url(""), "127.0.0.1"),
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
	resp, err := client.Get("http://" + host + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, string(body), "direct")
	assert.Equal(t, len(up.log()), 0)

	c := mustChain(t, "http://proxy.test:3128", "", ".corp.test")
	assert.Equal(t, c.For("https", "git.corp.test:443") == nil, true)
	assert.Equal(t, c.For("https", "example.com:443").Host, "proxy.test:3128")
	assert.Equal(t, c.For("http", "example.com:80") == nil, true) // no HTTP_PROXY: plain http goes direct
}

func TestChainUpstreamRefusal(t *testing.T) {
	up := newFakeUpstream(t, "", "u:right")
	purl, audit := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com"}},
		Chain:  mustChain(t, up.url("u:wrong"), up.url("u:wrong"), ""),
	})
	conn, _ := connect(t, purl.Host, "example.com:443")
	conn.Write(clientHello(t, "example.com"))
	_, _ = io.ReadAll(conn)
	conn.Close()
	ev := audit.last(t)
	assert.Equal(t, ev.Allowed, false)
	assert.Equal(t, ev.Reason, "upstream: 407")
	assert.Equal(t, len(up.log()), 1) // no direct retry, no second try

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
	resp, err := client.Get("http://example.com/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	assert.Equal(t, audit.last(t).Reason, "upstream: 407")
}

func TestChainPolicyFirst(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	purl, audit := chainedProxy(t, Options{
		Policy: Policy{Allow: []string{"example.com", "10.1.2.3"}},
		Chain:  mustChain(t, up.url(""), up.url(""), ""),
	})
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
	resp, err := client.Get("http://evil.test/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, audit.last(t).Rule, "default")

	// a literal private destination is the guard's, even behind the upstream.
	resp, err = client.Get("http://10.1.2.3/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, audit.last(t).Reason, "private")
	conn, _ := connect(t, purl.Host, "10.1.2.3:443")
	conn.Write(clientHello(t, ""))
	_, _ = io.ReadAll(conn)
	conn.Close()
	assert.Equal(t, audit.last(t).Reason, "private")
	// the upstream itself is on loopback, and reached: it is exempt.
	assert.Equal(t, len(up.log()), 0)
	resp, err = client.Get("http://example.com/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, len(up.log()), 1)
}

// a direct request that names the upstream's own address is guarded as any
// other: only the connection to the chosen upstream is exempt.
func TestChainDirectToUpstreamGuarded(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	host := strings.TrimPrefix(up.url(""), "http://")
	for name, c := range map[string]*Chain{
		"no http upstream": mustChain(t, up.url(""), "", ""),
		"no_proxy":         mustChain(t, up.url(""), up.url(""), "127.0.0.1"),
	} {
		t.Run(name, func(t *testing.T) {
			purl, audit := chainedProxy(t, Options{Policy: Policy{Allow: []string{host}}, Chain: c})
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
			resp, err := client.Get("http://" + host + "/")
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
			assert.Equal(t, audit.last(t).Reason, "private")
			assert.Equal(t, len(up.log()), 0)
		})
	}
}

func TestChainFromEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	c, err := ChainFromEnv(env("ALL_PROXY", "http://p:1", "all_proxy", "socks5://p:2"))
	require.NoError(t, err)
	assert.Equal(t, c == nil, true)

	_, err = ChainFromEnv(env("HTTPS_PROXY", "socks5://u:secret@p:1080"))
	require.Error(t, err, assert.AnyError)
	assert.ContainsString(t, err.Error(), "socks5")
	assert.That(t, !strings.Contains(err.Error(), "secret"), "credential in %q", err)

	for _, bad := range []string{"https://u:secret@", "http://u:secret@/x", "http://host:1/u:secret", "://u:secret@x"} {
		_, err = ChainFromEnv(env("HTTPS_PROXY", bad))
		require.Error(t, err, assert.AnyError)
		assert.That(t, !strings.Contains(err.Error(), "secret"), "credential in %q", err)
	}

	c, err = ChainFromEnv(env("https_proxy", "proxy.corp:3128", "no_proxy", ".corp"))
	require.NoError(t, err)
	assert.Equal(t, c.HTTPS.String(), "http://proxy.corp:3128")
	assert.Equal(t, c.NoProxy, ".corp")
	assert.Equal(t, c.HTTP == nil, true)

	c, err = ChainFromEnv(env("HTTPS_PROXY", "http://u:secret@proxy.corp:3128", "HTTP_PROXY", "http://u:secret@proxy.corp:3128"))
	require.NoError(t, err)
	assert.Equal(t, c.Describe(), "http://u:xxxxx@proxy.corp:3128")
	c, err = ChainFromEnv(env("HTTPS_PROXY", "http://a:1", "HTTP_PROXY", "http://b"))
	require.NoError(t, err)
	assert.Equal(t, c.Describe(), "http://a:1, http via http://b:80")

	// pats's own keys win over the machine's names.
	c, err = ChainFromEnv(env("HTTPS_PROXY", "http://machine:1", EnvUpstreamHTTPS, "http://pats:2"))
	require.NoError(t, err)
	assert.Equal(t, c.HTTPS.Host, "pats:2")
	back, err := ChainFromEnv(env(flatten(c.Env())...))
	require.NoError(t, err)
	assert.Equal(t, back.HTTPS.String(), c.HTTPS.String())

	assert.Equal(t, mustChain(t, "http://localhost:3128", "", "").Loopback() != nil, true)
	assert.Equal(t, mustChain(t, "http://[::1]:3128", "", "").Loopback() != nil, true)
	assert.Equal(t, mustChain(t, "http://10.0.0.1:3128", "", "").Loopback() == nil, true)
}

func flatten(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}

func TestServeUnreachableUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()
	var ready strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = Serve(ctx, Options{Listen: "127.0.0.1:0", Ready: &ready,
		Chain: mustChain(t, "http://u:secret@"+addr, "", "")})
	require.Error(t, err, assert.AnyError)
	assert.ContainsString(t, err.Error(), "upstream http://u:xxxxx@"+addr+" unreachable")
	assert.Equal(t, ready.String(), "")
}
