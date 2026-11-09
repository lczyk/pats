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
