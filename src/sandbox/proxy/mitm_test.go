package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// testCA mints a throwaway CA and writes it to dir, returning paths + cert pool.
func testCA(t testing.TB, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPath = filepath.Join(dir, "ca.pem")
	keyPath = filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	pool = x509.NewCertPool()
	cert, _ := x509.ParseCertificate(der)
	pool.AddCert(cert)
	return certPath, keyPath, pool
}

// inline pem (the run phase passes the CA over env, not a mount -- the
// rootless proxy has no fs access) must load the same as file paths.
func TestNewSignerInlinePEM(t *testing.T) {
	certPath, keyPath, _ := testCA(t, t.TempDir())
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	s, err := NewSigner(string(certPEM), string(keyPEM))
	require.NoError(t, err)
	_, err = s.leaf("example.com")
	require.NoError(t, err)
}

// COVER: end-to-end mitm path -- CONNECT terminates tls with a CA-signed leaf,
// allowed urls reach the upstream, denied urls die with 403 before leaving.
func TestMitmE2E(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello:"+r.URL.Path)
	}))
	defer upstream.Close()
	uhost := strings.TrimPrefix(upstream.URL, "https://")

	certPath, keyPath, pool := testCA(t, t.TempDir())
	s, err := NewSigner(certPath, keyPath)
	require.NoError(t, err)

	var audit auditBuf
	// the proxy's upstream transport trusts the test server's self-signed cert.
	p := &Proxy{
		Rule:     mustRule(t, false, []string{uhost}, []string{uhost + "/secret**"}),
		Signer:   s,
		Upstream: upstream.Client().Transport,
		Audit:    &audit,
	}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool}, // trusts only the mitm CA
	}}

	resp, err := client.Get(upstream.URL + "/ok?token=abc")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, string(body), "hello:/ok")
	// the audit sees the full url, query included.
	ev := audit.last(t)
	assert.Equal(t, ev.Ev, "req")
	assert.Equal(t, ev.Kind, "mitm")
	assert.Equal(t, ev.Allowed, true)
	assert.Equal(t, ev.Status, 200)
	assert.Equal(t, ev.Revocation, "unchecked") // the test server staples nothing
	assert.That(t, strings.HasSuffix(ev.URL, "/ok?token=abc"), "audit url %q", ev.URL)

	resp, err = client.Get(upstream.URL + "/secret/answer.yaml")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, audit.last(t).Rule, uhost+"/secret**")

	// non-mitm'd host on the same rule set stays a tunnel and is refused by
	// the host gate (not on the allowlist).
	resp, err = client.Get("https://203.0.113.1/x")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected host-gate refusal for unlisted host")
	}
}

// rtFunc adapts a func to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// mitmProxy is a proxy that mitm's host via a harmless deny entry and answers
// upstream requests with rt.
func mitmProxy(t testing.TB, host string, rt http.RoundTripper) (*Proxy, *x509.CertPool, *auditBuf) {
	t.Helper()
	certPath, keyPath, pool := testCA(t, t.TempDir())
	s, err := NewSigner(certPath, keyPath)
	require.NoError(t, err)
	var audit auditBuf
	p := &Proxy{
		Rule:     mustRule(t, false, []string{host}, []string{host + "/secret**"}),
		Signer:   s,
		Upstream: rt,
		Audit:    &audit,
	}
	return p, pool, &audit
}

// the mitm'd client connection is http/1.1 (no h2 in NextProtos), but the
// proxy's upstream leg may negotiate http/2 -- the response can't be written
// verbatim: the status line would read "HTTP/2.0 200 OK", and an unknown
// length (ContentLength -1) makes Write add Transfer-Encoding: chunked while
// a stale upstream Content-Length header line survives alongside (double
// framing). lenient parsers (curl, node, go) shrug both off; strict ones
// (rust hyper -- e.g. github copilot's auth stack) kill the request. this
// test reads the raw wire bytes, which the lenient go client would hide.
func TestMitmH2ResponseRewrite(t *testing.T) {
	// the pathological upstream shape: h2 proto, unknown length, and a stale
	// Content-Length header left in the map.
	h2upstream := rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Proto: "HTTP/2.0", ProtoMajor: 2, ProtoMinor: 0,
			Header:        http.Header{"Content-Length": {"2"}},
			Body:          io.NopCloser(strings.NewReader("hi")),
			ContentLength: -1,
		}, nil
	})
	p, pool, _ := mitmProxy(t, "mitm.test", h2upstream)
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	conn, _ := connect(t, proxy.Listener.Addr().String(), "mitm.test:443")
	defer conn.Close()

	tconn := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: "mitm.test"})
	require.NoError(t, tconn.Handshake())
	fmt.Fprintf(tconn, "GET /ok HTTP/1.1\r\nHost: mitm.test\r\n\r\n")

	tbr := bufio.NewReader(tconn)
	status, err := tbr.ReadString('\n')
	require.NoError(t, err)
	assert.That(t, strings.HasPrefix(status, "HTTP/1.1 200"), "status line %q", strings.TrimSpace(status))
	hasCL, hasTE := false, false
	for {
		line, err := tbr.ReadString('\n')
		require.NoError(t, err)
		if line == "\r\n" {
			break
		}
		l := strings.ToLower(line)
		hasCL = hasCL || strings.HasPrefix(l, "content-length:")
		hasTE = hasTE || strings.HasPrefix(l, "transfer-encoding:")
	}
	assert.That(t, !(hasCL && hasTE), "both Content-Length and Transfer-Encoding on the wire (double framing)")
	assert.That(t, hasCL || hasTE, "no framing header on the wire")
	// body survives the rewrite: chunked-decode if needed.
	var body []byte
	if hasTE {
		for {
			sizeLine, err := tbr.ReadString('\n')
			require.NoError(t, err)
			var n int
			_, err = fmt.Sscanf(strings.TrimSpace(sizeLine), "%x", &n)
			require.NoError(t, err)
			if n == 0 {
				break
			}
			chunk := make([]byte, n+2) // + trailing CRLF
			_, err = io.ReadFull(tbr, chunk)
			require.NoError(t, err)
			body = append(body, chunk[:n]...)
		}
	} else {
		body = make([]byte, 2)
		_, err = io.ReadFull(tbr, body)
		require.NoError(t, err)
	}
	assert.Equal(t, string(body), "hi")
}

// COVER: handleMitm's half of the raw-200-write fix -- see the matching
// comment on handleConnect's TestHandleConnectNoFramingHeaders in
// proxy_test.go. Go's http.Transport waits for the full CONNECT response
// before it starts its TLS handshake, so a lenient client never notices a
// regression here; the connect helper's raw read does.
func TestMitmNoFramingHeaders(t *testing.T) {
	p, _, _ := mitmProxy(t, "mitm.test", http.DefaultTransport)
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	conn, _ := connect(t, proxy.Listener.Addr().String(), "mitm.test:443")
	conn.Close()
}

// coalesceConn merges the first Write it sees with a pre-staged prefix
// (pending) into a single call to the underlying conn's Write -- one
// syscall, reproducing a client that pipelines its next message (here, a
// tls.Client's ClientHello) behind an earlier one (the CONNECT request line)
// without waiting for a reply. reads transparently discard the fixed CONNECT
// 200 response before handing bytes to whatever is layered on top, so a real
// tls.Client can read the handshake bytes that follow it.
type coalesceConn struct {
	net.Conn
	pending []byte
	flushed bool
	skipped bool
}

func (c *coalesceConn) Write(p []byte) (int, error) {
	if !c.flushed {
		c.flushed = true
		if _, err := c.Conn.Write(append(c.pending, p...)); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *coalesceConn) Read(p []byte) (int, error) {
	if !c.skipped {
		c.skipped = true
		want := []byte("HTTP/1.1 200 OK\r\n\r\n")
		got := make([]byte, len(want))
		if _, err := io.ReadFull(c.Conn, got); err != nil {
			return 0, err
		}
		if string(got) != string(want) {
			return 0, fmt.Errorf("coalesceConn: CONNECT response %q, want %q", got, want)
		}
	}
	return c.Conn.Read(p)
}

// COVER: handleMitm's half of the pipelined-bytes fix -- see the matching
// comment on handleConnect's TestHandleConnectPipelinedBytes in
// proxy_test.go. coalesceConn (above) forces the CONNECT request line and
// the client's TLS ClientHello into the same underlying Write call, so the
// bytes genuinely arrive together at the syscall level, the same way a real
// client pipelines its ClientHello without waiting for the CONNECT 200.
// handleMitm must read those bytes back out of Hijack's bufio.Reader (the
// bufConn wrapping) when building its tls.Server, or the handshake stalls
// forever waiting for bytes that already arrived.
func TestMitmPipelinedClientHello(t *testing.T) {
	p, pool, _ := mitmProxy(t, "mitm.test", http.DefaultTransport)
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	connectReq := "CONNECT mitm.test:443 HTTP/1.1\r\nHost: mitm.test:443\r\n\r\n"
	cc := &coalesceConn{Conn: conn, pending: []byte(connectReq)}

	tconn := tls.Client(cc, &tls.Config{RootCAs: pool, ServerName: "mitm.test"})
	defer tconn.Close()
	require.NoError(t, tconn.Handshake())
	assert.That(t, tconn.ConnectionState().HandshakeComplete)
}

// the leaf is only ever for the CONNECT host: a hello naming another host
// fails its handshake, and an inner request whose Host names another host is
// refused before it reaches upstream.
func TestMitmHostBinding(t *testing.T) {
	calls := 0
	rt := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
			Body: http.NoBody, ContentLength: 0}, nil
	})
	p, pool, audit := mitmProxy(t, "mitm.test", rt)
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	paddr := proxy.Listener.Addr().String()

	conn, _ := connect(t, paddr, "mitm.test:443")
	tconn := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: "other.test"})
	assert.Error(t, tconn.Handshake(), assert.AnyError)
	conn.Close()
	assert.Equal(t, audit.last(t).Reason, "sni-mismatch")

	conn, _ = connect(t, paddr, "mitm.test:443")
	defer conn.Close()
	tconn = tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: "mitm.test"})
	require.NoError(t, tconn.Handshake())
	fmt.Fprintf(tconn, "GET /x HTTP/1.1\r\nHost: other.test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tconn), nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, audit.last(t).Reason, "host-mismatch")
	assert.Equal(t, calls, 0)
}

// a connection forced into mitm that never speaks tls (an ssh-style banner
// on an allowed port, under a methods posture) is refused on the record, not
// dropped silently.
func TestMitmNonTLSAudited(t *testing.T) {
	p, _, audit := mitmProxy(t, "mitm.test:22", http.DefaultTransport)
	p.Rule.Methods = map[string]bool{"GET": true}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	conn, br := connect(t, proxy.Listener.Addr().String(), "mitm.test:22")
	defer conn.Close()
	fmt.Fprintf(conn, "SSH-2.0-OpenSSH_9.6\r\n")
	_, _ = br.ReadByte()
	ev := audit.last(t)
	assert.Equal(t, ev.Ev, "open")
	assert.Equal(t, ev.Allowed, false)
	assert.Equal(t, ev.Reason, "not-tls")
}

// mitmConn opens a connection to host through p, terminated by p's CA.
func mitmConn(t testing.TB, p *Proxy, pool *x509.CertPool, host string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	conn, _ := connect(t, srv.Listener.Addr().String(), host+":443")
	t.Cleanup(func() { conn.Close() })
	tconn := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: host})
	require.NoError(t, tconn.Handshake())
	return tconn, bufio.NewReader(tconn)
}

// answer reads one response off a mitm'd connection, body and all.
func answer(t testing.TB, br *bufio.Reader) (*http.Response, string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, string(body)
}

// echoPath answers every request with its path, and reads no request body.
var echoPath = rtFunc(func(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"text/plain"}},
		Body: io.NopCloser(strings.NewReader(r.URL.Path)), ContentLength: int64(len(r.URL.Path))}, nil
})

// a request body the origin never read is not the next request: the two
// requests of one connection are each decided and answered as themselves.
func TestMitmUnreadBody(t *testing.T) {
	p, pool, audit := mitmProxy(t, "mitm.test", echoPath)
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "POST /a HTTP/1.1\r\nHost: mitm.test\r\nContent-Length: 14\r\n\r\nGET /secret/x GET /b HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	_, body := answer(t, br)
	assert.Equal(t, body, "/a")
	resp, body := answer(t, br)
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, body, "/b")
	var urls []string
	for _, ev := range audit.events(t) {
		urls = append(urls, ev.Method+" "+ev.URL)
	}
	assert.EqualArrays(t, urls, []string{"POST mitm.test/a", "GET mitm.test/b"})
}

// what a client says to the proxy stays with the proxy, and the rest goes on
// as it was sent and decided on: the query as spelt, the client's own
// forwarding headers, no user agent where it sent none.
func TestMitmForwardsAsSent(t *testing.T) {
	var got *http.Request
	var sent string
	var trailer http.Header
	p, pool, _ := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			sent, trailer = string(b), r.Trailer
		} else {
			got = r
		}
		return echoPath(r)
	}))
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "GET /v1/x?b=2;a=1&c=%%zz HTTP/1.1\r\nHost: mitm.test\r\nConnection: keep-alive, X-Hop\r\nX-Hop: secret\r\n"+
		"Proxy-Authorization: Basic abc\r\nKeep-Alive: timeout=5\r\nX-Forwarded-For: 10.1.2.3\r\nForwarded: for=10.1.2.3\r\nX-Kept: yes\r\n\r\n")
	answer(t, br)
	require.That(t, got != nil, "the request did not reach the origin")
	for _, h := range []string{"X-Hop", "Proxy-Authorization", "Keep-Alive", "Connection"} {
		assert.Equal(t, len(got.Header[h]), 0, "%s went on to the origin", h)
	}
	assert.EqualArrays(t, got.Header["X-Forwarded-For"], []string{"10.1.2.3"})
	assert.EqualArrays(t, got.Header["Forwarded"], []string{"for=10.1.2.3"})
	assert.Equal(t, got.Header.Get("X-Kept"), "yes")
	assert.Equal(t, got.Header.Get("User-Agent"), "")
	assert.Equal(t, got.URL.RawQuery, "b=2;a=1&c=%zz")

	// a trailer the client sends after a chunked body goes on with it.
	fmt.Fprintf(tconn, "POST /t HTTP/1.1\r\nHost: mitm.test\r\nTransfer-Encoding: chunked\r\nTrailer: X-T\r\n\r\n3\r\nabc\r\n0\r\nX-T: tv\r\n\r\n")
	answer(t, br)
	assert.Equal(t, sent, "abc")
	assert.Equal(t, trailer.Get("X-T"), "tv")
	assert.Equal(t, got.URL.String(), "https://mitm.test:443/v1/x?b=2;a=1&c=%zz")
	assert.Equal(t, got.Host, "mitm.test")
}

// the answer is the origin's: nothing it did not send is added, and a
// stream reaches the client as it comes, not when it ends.
func TestMitmAnswerAsSent(t *testing.T) {
	pr, pw := io.Pipe()
	p, pool, _ := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/stream" {
			return &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1,
				Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr, ContentLength: -1}, nil
		}
		return &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("<html><body>")), ContentLength: 12}, nil
	}))
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "GET /bare HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	resp, body := answer(t, br)
	assert.Equal(t, body, "<html><body>")
	assert.Equal(t, len(resp.Header["Content-Type"]), 0, "a content type was guessed: %v", resp.Header)
	assert.Equal(t, len(resp.Header["Date"]), 0, "a date was added: %v", resp.Header)

	fmt.Fprintf(tconn, "GET /stream HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	go func() { _, _ = pw.Write([]byte("data: one\n\n")) }()
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	first := make([]byte, len("data: one\n\n"))
	_, err = io.ReadFull(resp.Body, first) // the origin has not ended its answer yet
	require.NoError(t, err)
	assert.Equal(t, string(first), "data: one\n\n")
	go func() { _, _ = pw.Write([]byte("data: two\n\n")); pw.Close() }()
	rest, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, string(rest), "data: two\n\n")
}

// a refusal inside a terminated connection takes the policy's shape, and one
// that answers leaves the connection to the next request. an origin that
// could not be reached is a 502, and no more.
func TestMitmRefusals(t *testing.T) {
	for _, shape := range []string{"403", "404", "reset"} {
		t.Run(shape, func(t *testing.T) {
			fail := true
			p, pool, audit := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/down" && fail {
					return nil, &RefusedError{Reason: "private"}
				}
				return echoPath(r)
			}))
			p.DenyResponse = shape
			tconn, br := mitmConn(t, p, pool, "mitm.test")
			fmt.Fprintf(tconn, "GET /down HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
			resp, _ := answer(t, br)
			assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
			ev := audit.last(t)
			assert.Equal(t, ev.Reason, "private")
			assert.Equal(t, ev.Allowed, false)
			assert.Equal(t, ev.Status, http.StatusBadGateway)

			fmt.Fprintf(tconn, "POST /secret/x HTTP/1.1\r\nHost: mitm.test\r\nContent-Length: 3\r\n\r\nabc")
			if shape == "reset" {
				_, err := http.ReadResponse(br, nil)
				assert.Error(t, err, assert.AnyError)
				assert.Equal(t, audit.last(t).Status, 0)
				return
			}
			resp, _ = answer(t, br)
			assert.Equal(t, strconv.Itoa(resp.StatusCode), shape)
			assert.Equal(t, strconv.Itoa(audit.last(t).Status), shape)
			fmt.Fprintf(tconn, "GET /after HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
			resp, body := answer(t, br)
			assert.Equal(t, resp.StatusCode, 200)
			assert.Equal(t, body, "/after")
		})
	}
}

// a switch of protocols (a websocket) is not passed: the origin is not
// offered it, its plain answer comes back, and the connection goes on.
func TestMitmNoSwitchedProtocol(t *testing.T) {
	var got http.Header
	p, pool, audit := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header
		if r.URL.Path == "/099" {
			return &http.Response{StatusCode: 99, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: http.NoBody}, nil
		}
		if r.URL.Path == "/insists" {
			return &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1,
				Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, Body: http.NoBody}, nil
		}
		return echoPath(r)
	}))
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "GET /ws HTTP/1.1\r\nHost: mitm.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: x\r\n\r\n")
	resp, body := answer(t, br)
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, body, "/ws")
	assert.Equal(t, got.Get("Upgrade"), "")
	assert.Equal(t, got.Get("Connection"), "")

	// an origin that switches unasked is a failed exchange, not a tunnel,
	// and so is one whose status is no status.
	fmt.Fprintf(tconn, "GET /insists HTTP/1.1\r\nHost: mitm.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	resp, body = answer(t, br)
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	assert.Equal(t, body, "egress: upgrade\n")
	ev := audit.last(t)
	assert.Equal(t, ev.Status, http.StatusBadGateway)
	assert.Equal(t, ev.Allowed, false)
	assert.Equal(t, ev.Reason, "upgrade")
	fmt.Fprintf(tconn, "GET /099 HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	resp, _ = answer(t, br)
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	fmt.Fprintf(tconn, "GET /after HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	_, body = answer(t, br)
	assert.Equal(t, body, "/after")
}

// "OPTIONS *" is a request like any other: decided, relayed and audited,
// not answered by the server on the origin's behalf.
func TestMitmOptionsStar(t *testing.T) {
	var got *http.Request
	p, pool, audit := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return echoPath(r)
	}))
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "OPTIONS * HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	resp, _ := answer(t, br)
	assert.Equal(t, resp.StatusCode, 200)
	require.That(t, got != nil, "the origin was not asked")
	assert.Equal(t, got.Method+" "+got.URL.Path, "OPTIONS *")
	ev := audit.last(t)
	assert.Equal(t, ev.Method+" "+ev.URL, "OPTIONS mitm.test*")
	assert.Equal(t, ev.Allowed, true)
}

// a client that hangs up on a request it was allowed is not a refusal, and
// no failure of the way out: the origin's work is called off, and the audit
// says who left.
func TestMitmClientGone(t *testing.T) {
	asked := make(chan struct{})
	p, pool, audit := mitmProxy(t, "mitm.test", rtFunc(func(r *http.Request) (*http.Response, error) {
		close(asked)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	tconn, _ := mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "GET /slow HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	<-asked
	tconn.NetConn().Close()
	ev := audit.last(t)
	assert.Equal(t, ev.URL, "mitm.test/slow")
	assert.Equal(t, ev.Reason, "client-gone")
	assert.Equal(t, ev.Allowed, true)
	assert.Equal(t, ev.Status, 0)
}

// a terminated connection that says nothing is not held for ever: before
// its first request, and between two.
func TestMitmIdle(t *testing.T) {
	p, pool, _ := mitmProxy(t, "mitm.test", echoPath)
	p.Idle = 200 * time.Millisecond
	tconn, br := mitmConn(t, p, pool, "mitm.test")
	_, err := br.ReadByte()
	assert.ErrorIs(t, err, io.EOF)

	tconn, br = mitmConn(t, p, pool, "mitm.test")
	fmt.Fprintf(tconn, "GET /a HTTP/1.1\r\nHost: mitm.test\r\n\r\n")
	_, body := answer(t, br)
	assert.Equal(t, body, "/a")
	_, err = br.ReadByte()
	assert.ErrorIs(t, err, io.EOF)
}
