package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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

// auditBuf collects audit lines for assertions. the proxy writes from its
// handler goroutines, so reads take the same lock.
type auditBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	seen int // lines consumed by last
}

func (a *auditBuf) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *auditBuf) events(t *testing.T) []Event {
	t.Helper()
	a.mu.Lock()
	text := a.buf.String()
	a.mu.Unlock()
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var ev Event
		require.NoError(t, json.Unmarshal([]byte(line), &ev))
		out = append(out, ev)
	}
	return out
}

// last waits briefly for a new open or req line (close lines land whenever a
// tunnel ends, so they are left to closes): the proxy may audit a refusal
// after the client has already seen the connection drop.
func (a *auditBuf) last(t *testing.T) Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var evs []Event
		for _, ev := range a.events(t) {
			if ev.Ev != "close" {
				evs = append(evs, ev)
			}
		}
		if n := len(evs); n > a.seen || time.Now().After(deadline) {
			require.That(t, n > 0, "no audit lines")
			a.seen = n
			return evs[n-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closes waits briefly for n close lines and returns them.
func (a *auditBuf) closes(t *testing.T, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var out []Event
		for _, ev := range a.events(t) {
			if ev.Ev == "close" {
				out = append(out, ev)
			}
		}
		if len(out) >= n || time.Now().After(deadline) {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// COVER: the plain-http proxy path (absolute-URI requests, e.g. apt) -- host
// gate, url entries applied directly (no mitm needed), and forwarding.
func TestHandleHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain:"+r.URL.Path)
	}))
	defer upstream.Close()
	uhost := strings.TrimPrefix(upstream.URL, "http://")

	var audit auditBuf
	p := &Proxy{Rule: mustRule(t, false, []string{uhost}, []string{uhost + "/secret**"}), Audit: &audit, Dial: looseDial}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL + "/ok?x=1")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, string(body), "plain:/ok")
	ev := audit.last(t)
	assert.Equal(t, ev.Ev, "req")
	assert.Equal(t, ev.Kind, "http")
	assert.Equal(t, ev.Allowed, true)
	assert.Equal(t, ev.Method, "GET")
	assert.Equal(t, ev.Status, 200)
	assert.Equal(t, ev.In, int64(len("plain:/ok")))
	assert.That(t, strings.HasPrefix(ev.UA, "Go-http-client"), "ua %q", ev.UA)
	assert.That(t, strings.HasSuffix(ev.URL, "/ok?x=1"), "audit url %q", ev.URL)
	assert.Equal(t, ev.Rule, uhost)

	resp, err = client.Get(upstream.URL + "/secret/x")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, audit.last(t).Rule, uhost+"/secret**")

	// unlisted host dies at the host gate.
	resp, err = client.Get("http://203.0.113.1/x")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, audit.last(t).Rule, "default")
}

func TestDenyResponse(t *testing.T) {
	for _, shape := range []string{"403", "404", "reset"} {
		t.Run(shape, func(t *testing.T) {
			p := &Proxy{Rule: mustRule(t, false, nil, nil), DenyResponse: shape}
			proxy := httptest.NewServer(p)
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
			resp, err := client.Get("http://203.0.113.1/x")
			switch shape {
			case "reset":
				assert.Error(t, err, assert.AnyError)
			default:
				require.NoError(t, err)
				resp.Body.Close()
				want := http.StatusForbidden
				if shape == "404" {
					want = http.StatusNotFound
				}
				assert.Equal(t, resp.StatusCode, want)
			}
		})
	}
}

// newEchoListener starts a raw tcp listener that echoes back whatever it
// reads -- something the CONNECT tunnel can dial straight through to without
// needing a full http server on the far end.
func newEchoListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln
}

// looseDial lets a test proxy reach its loopback upstreams.
var looseDial = Dialer(&Guard{AllowPrivate: true}).DialContext

// dialTo is a Proxy.Dial that ignores the target and connects to addr, so a
// test can CONNECT to a hostname without resolving it.
func dialTo(addr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
}

// connect sends a CONNECT for target through the proxy and returns the conn
// once the 200 and its blank line were read.
func connect(t *testing.T, proxyAddr, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(status), "HTTP/1.1 200 OK")
	blank, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, blank, "\r\n")
	return conn, br
}

// COVER: RFC 9110 9.3.6 forbids Transfer-Encoding/Content-Length on a 2xx
// response to CONNECT -- net/http's normal WriteHeader path flushes both once
// hijacked, so the status line must be written straight to the raw conn. the
// helper above asserts the 200 goes straight to the blank line.
func TestHandleConnectNoFramingHeaders(t *testing.T) {
	ln := newEchoListener(t)
	defer ln.Close()
	echoAddr := ln.Addr().String()

	proxy := httptest.NewServer(&Proxy{Rule: mustRule(t, false, []string{echoAddr}, nil), Dial: looseDial})
	defer proxy.Close()
	conn, _ := connect(t, proxy.Listener.Addr().String(), echoAddr)
	conn.Close()
}

// COVER: a client may pipeline bytes right behind the CONNECT request line
// without waiting for the 200 -- those bytes land in Hijack's bufio.Reader and
// must not be dropped, or the tunnel stalls. a non-tls opening on a port other
// than 443 is piped as-is.
func TestHandleConnectPipelinedBytes(t *testing.T) {
	ln := newEchoListener(t)
	defer ln.Close()
	echoAddr := ln.Addr().String()

	var audit auditBuf
	proxy := httptest.NewServer(&Proxy{Rule: mustRule(t, false, []string{echoAddr}, nil), Audit: &audit, Dial: looseDial})
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	// single write: request line, headers, and payload all at once, so the
	// payload is sitting in the server's read buffer before hijack ever runs.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nping", echoAddr, echoAddr)

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(status), "HTTP/1.1 200 OK")
	blank, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, blank, "\r\n")

	got := make([]byte, 4)
	_, err = io.ReadFull(br, got)
	require.NoError(t, err)
	assert.Equal(t, string(got), "ping")
	open := audit.last(t)
	assert.Equal(t, open.Ev, "open")
	assert.Equal(t, open.Kind, "raw") // not tls, not on 443
	assert.Equal(t, open.Allowed, true)
	assert.Equal(t, open.IP, "127.0.0.1")

	// the tunnel's close line carries the bytes each way.
	conn.Close()
	cl := audit.closes(t, 1)
	require.Equal(t, len(cl), 1)
	assert.Equal(t, cl[0].Out, int64(4))
	assert.Equal(t, cl[0].In, int64(4))
	assert.Equal(t, cl[0].Allowed, true)
}

// clientHello captures the ClientHello a go tls client sends for serverName.
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		_ = tls.Client(c1, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	}()
	buf := make([]byte, maxHello)
	n, err := c2.Read(buf)
	require.NoError(t, err)
	return buf[:n]
}

func TestParseClientHello(t *testing.T) {
	raw := clientHello(t, "good.test")
	need := 4 + (int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8]))
	sni, ech, err := parseClientHello(raw[9 : 5+need])
	require.NoError(t, err)
	assert.Equal(t, sni, "good.test")
	assert.Equal(t, ech, false)

	raw = clientHello(t, "")
	need = 4 + (int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8]))
	sni, _, err = parseClientHello(raw[9 : 5+need])
	require.NoError(t, err)
	assert.Equal(t, sni, "")

	_, _, err = parseClientHello([]byte{1, 2, 3})
	assert.Error(t, err, assert.AnyError)
}

// fragment splits one tls record into two records at off.
func fragment(rec []byte, off int) []byte {
	body := rec[5:]
	a, b := body[:off], body[off:]
	out := append([]byte{}, rec[:3]...)
	out = append(out, byte(len(a)>>8), byte(len(a)))
	out = append(out, a...)
	out = append(out, rec[:3]...)
	out = append(out, byte(len(b)>>8), byte(len(b)))
	out = append(out, b...)
	return out
}

// the tunnel holds the ClientHello to the CONNECT host: a hello naming
// another host is refused before any byte reaches upstream, one naming the
// right host (even split across records) is replayed unchanged, and ech or a
// missing sni on a named host is refused.
func TestHandleConnectSNI(t *testing.T) {
	ln := newEchoListener(t)
	defer ln.Close()

	var audit auditBuf
	p := &Proxy{Rule: mustRule(t, false, []string{"good.test", "1.2.3.4"}, nil), Dial: dialTo(ln.Addr().String()), Audit: &audit}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	paddr := proxy.Listener.Addr().String()

	send := func(t *testing.T, target string, hello []byte) ([]byte, error) {
		conn, br := connect(t, paddr, target)
		defer conn.Close()
		_, err := conn.Write(hello)
		require.NoError(t, err)
		got := make([]byte, len(hello))
		_, err = io.ReadFull(br, got)
		return got, err
	}

	hello := clientHello(t, "good.test")
	got, err := send(t, "good.test:443", hello)
	require.NoError(t, err)
	assert.That(t, bytes.Equal(got, hello), "hello not replayed verbatim")
	assert.Equal(t, audit.last(t).Allowed, true)

	got, err = send(t, "good.test:443", fragment(hello, 20))
	require.NoError(t, err)
	assert.That(t, bytes.Equal(got, fragment(hello, 20)), "fragmented hello not replayed verbatim")

	_, err = send(t, "good.test:443", clientHello(t, "evil.test"))
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, audit.last(t).Reason, "sni-mismatch")

	_, err = send(t, "good.test:443", clientHello(t, ""))
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, audit.last(t).Reason, "no-sni")

	// an ip-literal CONNECT host carries no sni by nature.
	_, err = send(t, "1.2.3.4:443", clientHello(t, ""))
	require.NoError(t, err)

	// not tls at all on 443 is refused.
	_, err = send(t, "good.test:443", []byte("GET / HTTP/1.1\r\n\r\n"))
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, audit.last(t).Reason, "not-tls")

	// an ech extension hides the real name: refused.
	echHello := append([]byte{}, hello...)
	// append an empty extension of type 0xfe0d and fix the lengths up.
	echHello = append(echHello, 0xfe, 0x0d, 0, 0)
	fix := func(b []byte, at, delta int, width int) {
		n := 0
		for i := 0; i < width; i++ {
			n = n<<8 | int(b[at+i])
		}
		n += delta
		for i := width - 1; i >= 0; i-- {
			b[at+i] = byte(n)
			n >>= 8
		}
	}
	fix(echHello, 3, 4, 2) // record length
	fix(echHello, 6, 4, 3) // handshake length
	// extensions length: after version(2) random(32) sessid(1+n) ciphers(2+n) comp(1+n)
	off := 9 + 2 + 32
	off += 1 + int(echHello[off])
	off += 2 + (int(echHello[off])<<8 | int(echHello[off+1]))
	off += 1 + int(echHello[off])
	fix(echHello, off, 4, 2)
	_, err = send(t, "good.test:443", echHello)
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, audit.last(t).Reason, "ech")
}

// a dial the guard refuses is audited with its reason and the tunnel closed.
func TestHandleConnectGuard(t *testing.T) {
	var audit auditBuf
	p := &Proxy{Rule: mustRule(t, false, []string{"127.0.0.1:1"}, nil), Audit: &audit}
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	conn, br := connect(t, proxy.Listener.Addr().String(), "127.0.0.1:1")
	defer conn.Close()
	_, _ = conn.Write([]byte("x"))
	_, err := br.ReadByte()
	assert.Error(t, err, assert.AnyError)
	assert.Equal(t, audit.last(t).Reason, "private")
}
