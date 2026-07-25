package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestPermits(t *testing.T) {
	deny := Rule{DefaultAllow: false, Allow: []string{"api.anthropic.com", ".ubuntu.com", "*.npmjs.org"}}
	allow := Rule{DefaultAllow: true, Deny: []string{"github.com", ".githubusercontent.com"}}

	cases := []struct {
		r    Rule
		host string
		want bool
	}{
		{deny, "api.anthropic.com", true},           // exact allow
		{deny, "archive.ubuntu.com", true},          // suffix allow
		{deny, "ubuntu.com", true},                  // bare suffix root
		{deny, "registry.npmjs.org", true},          // *.suffix
		{deny, "github.com", false},                 // not in allowlist
		{deny, "evil.com", false},                   // default deny
		{allow, "example.com", true},                // default allow
		{allow, "github.com", false},                // exact deny
		{allow, "raw.githubusercontent.com", false}, // suffix deny
	}
	for _, c := range cases {
		assert.Equal(t, c.r.permits(c.host), c.want)
	}
}

// COVER: the plain-http proxy path (absolute-URI requests, e.g. apt) -- host
// gate, url rules applied directly (no mitm needed), and forwarding.
func TestHandleHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain:"+r.URL.Path)
	}))
	defer upstream.Close()

	r := Rule{
		Allow:    []string{"127.0.0.1"},
		DenyURLs: ParseURLRules([]string{"127.0.0.1/secret*"}),
	}
	proxy := httptest.NewServer(Handler(r, nil, nil))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL + "/ok")
	if err != nil {
		require.NoError(t, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, string(body), "plain:/ok")

	resp, err = client.Get(upstream.URL + "/secret/x")
	if err != nil {
		require.NoError(t, err)
	}
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)

	// unlisted host dies at the host gate.
	resp, err = client.Get("http://203.0.113.1/x")
	if err != nil {
		require.NoError(t, err)
	}
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
}

// newEchoListener starts a raw tcp listener that echoes back whatever it
// reads -- something the blind CONNECT tunnel can dial straight through to
// without needing a full http server on the far end.
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

// COVER: RFC 9110 9.3.6 forbids Transfer-Encoding/Content-Length on a 2xx
// response to CONNECT -- net/http's normal WriteHeader path flushes both once
// hijacked, so the status line must be written straight to the raw conn.
func TestHandleConnectNoFramingHeaders(t *testing.T) {
	ln := newEchoListener(t)
	defer ln.Close()
	echoAddr := ln.Addr().String()
	echoHost, _, _ := net.SplitHostPort(echoAddr)

	r := Rule{Allow: []string{echoHost}}
	proxy := httptest.NewServer(Handler(r, nil, nil))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(status), "HTTP/1.1 200 OK")

	var headers []string
	for {
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		if line == "\r\n" {
			break
		}
		headers = append(headers, strings.ToLower(line))
	}
	for _, h := range headers {
		if strings.HasPrefix(h, "content-length:") || strings.HasPrefix(h, "transfer-encoding:") {
			t.Fatalf("CONNECT 200 carries a framing header: %q", h)
		}
	}
}

// COVER: a client may pipeline bytes right behind the CONNECT request line
// (e.g. a TLS ClientHello) without waiting for the 200 -- those bytes land in
// Hijack's bufio.Reader and must not be dropped, or the tunnel stalls.
func TestHandleConnectPipelinedBytes(t *testing.T) {
	ln := newEchoListener(t)
	defer ln.Close()
	echoAddr := ln.Addr().String()
	echoHost, _, _ := net.SplitHostPort(echoAddr)

	r := Rule{Allow: []string{echoHost}}
	proxy := httptest.NewServer(Handler(r, nil, nil))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

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
}
