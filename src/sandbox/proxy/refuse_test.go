package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// exchange sends request to addr and returns all the peer wrote before it
// closed the connection. a peer that keeps the connection fails the read.
func exchange(t testing.TB, addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.WriteString(conn, request)
	require.NoError(t, err)
	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	return string(got)
}

// forbidHead is the head of a 403 refusal whose body is n bytes.
func forbidHead(n int) string {
	return "HTTP/1.1 403 Blocked by network policy, do not retry\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", n) +
		"Connection: close\r\n\r\n"
}

// a 403 refusal is the same bytes for a refused CONNECT and a refused
// request, whatever version asked: the phrase, the message, and a close.
func TestRefusalOnTheWire(t *testing.T) {
	var audit auditBuf
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, nil, nil), Audit: &audit})
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	assert.Equal(t, len(DefaultDenyMessage), 250)
	want := forbidHead(250) + DefaultDenyMessage
	for name, req := range map[string]string{
		"connect":  "CONNECT evil.test:443 HTTP/1.1\r\nHost: evil.test:443\r\n\r\n",
		"plain":    "GET http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\n\r\n",
		"http/1.0": "GET http://evil.test/x HTTP/1.0\r\n\r\n",
		// a body under the drain is read off, and the answer stands.
		"post": "POST http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\nContent-Length: 3\r\n\r\nabc",
		// what was pipelined behind a refusal goes with its connection.
		"pipelined": "GET http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\n\r\nGET http://evil.test/y HTTP/1.1\r\nHost: evil.test\r\n\r\n",
	} {
		assert.Equal(t, exchange(t, addr, req), want, name)
	}
	// a HEAD is told the length and sent no body.
	assert.Equal(t, exchange(t, addr, "HEAD http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\n\r\n"), forbidHead(250))
	// one line for each refusal, and none for what was pipelined behind one.
	evs := audit.events(t)
	assert.Len(t, evs, 6)
	for _, ev := range evs {
		assert.Equal(t, ev.Status, http.StatusForbidden, ev.URL)
		assert.Equal(t, ev.Allowed, false)
		assert.That(t, !strings.HasSuffix(ev.URL, "/y"), "the pipelined request was served")
	}
}

// deadConn is a connection whose every write fails.
type deadConn struct{ net.Conn }

func (deadConn) Write([]byte) (int, error) { return 0, errors.New("the client is gone") }

// hijackable is a response writer that hands over conn.
type hijackable struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// every way of refusing leaves one audit line with the status decided on,
// whether or not the answer could be written.
func TestRefusalAuditedOnce(t *testing.T) {
	bare := ""
	for _, c := range []struct {
		response string
		message  *string
		status   int
	}{
		{"", nil, 403}, {"403", nil, 403}, {"403", &bare, 403}, {"404", nil, 404}, {"reset", nil, 0},
	} {
		var audit auditBuf
		p := &Proxy{DenyResponse: c.response, DenyMessage: c.message, Audit: &audit}
		p.init()
		client, server := net.Pipe()
		client.Close()
		w := hijackable{httptest.NewRecorder(), deadConn{server}}
		p.refuse(w, httptest.NewRequest("GET", "http://evil.test/x", nil), Event{Ev: "req", Host: "evil.test"})
		evs := audit.events(t)
		require.Len(t, evs, 1, c.response)
		assert.Equal(t, evs[0].Status, c.status, c.response)
		assert.Equal(t, evs[0].Allowed, false)
	}
}

// a deny-message is the body as written, a final newline added, and its
// length is counted in bytes. the phrase stays the default's.
func TestRefusalMessageSet(t *testing.T) {
	msg := "no acc\u00e8s"
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, nil, nil), DenyMessage: &msg})
	defer srv.Close()
	got := exchange(t, srv.Listener.Addr().String(), "GET http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\n\r\n")
	assert.Equal(t, got, forbidHead(10)+"no acc\u00e8s\n")
}

// an empty deny-message is the refusal as it was: net/http's own 403, which
// keeps the connection for the next request.
func TestRefusalMessageEmpty(t *testing.T) {
	none := ""
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, nil, nil), DenyMessage: &none})
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	br := bufio.NewReader(conn)
	for range 2 {
		fmt.Fprintf(conn, "GET http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\n\r\n")
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, resp.Status, "403 Forbidden")
		assert.Equal(t, string(body), "egress denied\n")
	}
}

// the answer does not wait on a body: a sender that stalls is answered and
// closed within the linger, and one waiting on a 100 gets the 403.
func TestRefusalDoesNotWaitOnABody(t *testing.T) {
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, nil, nil)})
	defer srv.Close()
	for name, req := range map[string]string{
		"stalled": "POST http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\nContent-Length: 1000000\r\n\r\nab",
		"expect":  "POST http://evil.test/x HTTP/1.1\r\nHost: evil.test\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n",
	} {
		start := time.Now()
		got := exchange(t, srv.Listener.Addr().String(), req)
		assert.Equal(t, got, forbidHead(250)+DefaultDenyMessage, name)
		assert.That(t, time.Since(start) < 3*time.Second, "%s: the refusal took %s", name, time.Since(start))
	}
}

// a refusal is audited before it is answered: a client that hangs up without
// reading leaves its line all the same.
func TestRefusalAuditedUnread(t *testing.T) {
	var audit auditBuf
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, nil, nil), Audit: &audit})
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	fmt.Fprintf(conn, "GET http://evil.test/gone HTTP/1.1\r\nHost: evil.test\r\n\r\n")
	conn.Close()
	ev := audit.last(t)
	assert.Equal(t, ev.URL, "evil.test/gone")
	assert.Equal(t, ev.Allowed, false)
	assert.Equal(t, ev.Status, http.StatusForbidden)
}

// a refusal after an allowed request, on one connection: the first is
// relayed, the second answered and the connection closed.
func TestRefusalAfterAnAllowedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer upstream.Close()
	uhost := strings.TrimPrefix(upstream.URL, "http://")
	srv := httptest.NewServer(&Proxy{Rule: mustRule(t, false, []string{uhost}, []string{uhost + "/secret**"}), Dial: looseDial})
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	br := bufio.NewReader(conn)
	fmt.Fprintf(conn, "GET http://%s/a HTTP/1.1\r\nHost: %s\r\n\r\n", uhost, uhost)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, string(body), "ok")
	fmt.Fprintf(conn, "GET http://%s/secret/x HTTP/1.1\r\nHost: %s\r\n\r\n", uhost, uhost)
	rest, err := io.ReadAll(br)
	require.NoError(t, err)
	assert.Equal(t, string(rest), forbidHead(250)+DefaultDenyMessage)
}

func TestDenyMessagePolicy(t *testing.T) {
	msg := func(s string) *string { return &s }
	// absent is the default text, spelled out; an empty one and one that is
	// set stay as written, and a policy that refuses another way has none.
	assert.Equal(t, *Policy{}.Resolved().DenyMessage, DefaultDenyMessage)
	assert.Equal(t, *Policy{DenyResponse: "403"}.Resolved().DenyMessage, DefaultDenyMessage)
	assert.Equal(t, *Policy{DenyMessage: msg("")}.Resolved().DenyMessage, "")
	assert.Equal(t, *Policy{DenyMessage: msg("x")}.Resolved().DenyMessage, "x")
	assert.That(t, Policy{DenyResponse: "404"}.Resolved().DenyMessage == nil)

	for _, ok := range []string{"", "one line", "two\nlines\n", strings.Repeat("x", 2048), "acc\u00e8s"} {
		_, err := Policy{DenyMessage: msg(ok)}.Rule()
		assert.NoError(t, err)
	}
	for bad, want := range map[string]string{
		strings.Repeat("x", 2049): "2049 bytes",
		"a\tb":                    "control character",
		"a\r\nb":                  "control character",
		"a\x1b[31mb":              "control character",
		"a\xffb":                  "not valid utf-8",
	} {
		_, err := Policy{DenyMessage: msg(bad)}.Rule()
		assert.Error(t, err, want)
	}
	for _, resp := range []string{"404", "reset", "timeout"} {
		_, err := Policy{DenyResponse: resp, DenyMessage: msg("x")}.Rule()
		assert.Error(t, err, "deny-message needs deny-response 403")
	}

	// a policy file: resolved as it is read, a null refused.
	p, err := ParsePolicy([]byte("allow: [a.test]\n"))
	require.NoError(t, err)
	assert.Equal(t, *p.DenyMessage, DefaultDenyMessage)
	p, err = ParsePolicy([]byte("deny-message: \"\"\n"))
	require.NoError(t, err)
	assert.Equal(t, *p.DenyMessage, "")
	p, err = ParsePolicy([]byte("deny-message: |\n  none of that here.\n"))
	require.NoError(t, err)
	assert.Equal(t, *p.DenyMessage, "none of that here.\n")
	for _, null := range []string{
		"deny-message:\n", "deny-message: null\n", "deny-message: ~\n",
		// through an alias, and with a merge.
		"harness: &n ~\ndeny-message: *n\n",
		"allow-private: &m {deny-message: ~}\n<<: *m\n",
	} {
		_, err = ParsePolicy([]byte(null))
		assert.Error(t, err, "null is neither a message nor none")
	}
}
