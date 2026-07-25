// Package proxy is a filtering forward proxy for sandboxed processes: CONNECT
// for https, absolute-URI for http, allow/deny by host, and (for hosts with
// url rules) tls termination with a per-run CA so requests are filtered by
// full url. every request is audited as one json line to stdout. the env
// protocol (PROXY_*) lives in cmd/egress-proxy; this package is the engine.
package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Rule is one egress policy: a default verdict plus host and url exceptions.
type Rule struct {
	DefaultAllow bool     // true = default allow
	Allow        []string // exceptions to deny-default
	Deny         []string // exceptions to allow-default
	DenyURLs     []URLRule
	AllowURLs    []URLRule // per-host allowlist: a host with rules only passes matching urls
}

func (r Rule) permits(host string) bool {
	if r.DefaultAllow { // default allow -> blocked iff in deny
		return !matchAny(host, r.Deny)
	}
	return matchAny(host, r.Allow) // default deny -> allowed iff in allow
}

func matchAny(host string, pats []string) bool {
	host = strings.ToLower(host)
	for _, p := range pats {
		p = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(p), "*"))
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, ".") {
			if host == p[1:] || strings.HasSuffix(host, p) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

func audit(host, port string, allowed bool) {
	// one json line per request -- the caller collects these as the egress log.
	b, _ := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "host": host, "port": port, "allowed": allowed,
	})
	os.Stdout.Write(append(b, '\n'))
}

// auditURL is audit + the full url -- only mitm'd requests have one.
func auditURL(host, port, url string, allowed bool) {
	b, _ := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "host": host, "port": port, "allowed": allowed, "url": url,
	})
	os.Stdout.Write(append(b, '\n'))
}

// Handler serves the proxy: CONNECT tunnels (mitm'd for hosts with url rules
// when s is non-nil), absolute-URI plain http otherwise. upstream is the
// RoundTripper for mitm'd requests (http.DefaultTransport in production).
func Handler(r Rule, s *Signer, upstream http.RoundTripper) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			handleConnect(w, req, r, s, upstream)
			return
		}
		handleHTTP(w, req, r)
	}
}

// handleConnect tunnels https. the target is req.Host (host:port). hosts with
// url rules are not tunneled blind -- their tls terminates here (mitm) so each
// request can be filtered by full url.
func handleConnect(w http.ResponseWriter, req *http.Request, r Rule, s *Signer, upstream http.RoundTripper) {
	host, port := splitHostPort(req.Host)
	if !r.permits(host) {
		audit(host, port, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	if s != nil && r.mitmHost(host) {
		handleMitm(w, req, r, s, upstream)
		return
	}
	audit(host, port, true)

	// dial before hijacking: a failed dial must still surface as a proper 502
	// via the ResponseWriter, which is unusable once hijacked.
	remote, err := net.DialTimeout("tcp", req.Host, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		remote.Close()
		return
	}
	client, crw, err := hj.Hijack()
	if err != nil {
		remote.Close()
		return
	}
	// the 200 must go straight to the wire: net/http's normal WriteHeader path
	// flushes a full response (Date, Transfer-Encoding: chunked, ...) once
	// anything is written after hijack, and RFC 9110 9.3.6 forbids framing
	// headers on a 2xx CONNECT response -- strict parsers (rust hyper) reject
	// the tunnel over it.
	if _, err := client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		client.Close()
		remote.Close()
		return
	}
	// crw.Reader may already hold bytes the client pipelined behind the
	// CONNECT request (e.g. a TLS ClientHello sent without waiting for the
	// 200) -- read through it, or those bytes are silently dropped and the
	// tunnel stalls.
	go pipe(remote, bufConn{Conn: client, r: crw.Reader})
	go pipe(client, remote)
}

// handleHTTP forwards plain http (absolute-URI proxy requests, e.g. apt).
func handleHTTP(w http.ResponseWriter, req *http.Request, r Rule) {
	host, port := splitHostPort(req.Host)
	if port == "" {
		port = "80"
	}
	if !r.permits(host) {
		audit(host, port, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	// plain http shows the path w/out mitm -- url rules apply directly, and the
	// audit gets the full url (query included) for free.
	fullURL := host + req.URL.Path
	if req.URL.RawQuery != "" {
		fullURL += "?" + req.URL.RawQuery
	}
	if !r.permitsURL(host, req.URL.Path, req.URL.RawQuery) {
		auditURL(host, port, fullURL, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	auditURL(host, port, fullURL, true)

	req.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func splitHostPort(hp string) (host, port string) {
	if h, p, err := net.SplitHostPort(hp); err == nil {
		return h, p
	}
	return hp, ""
}

func pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	io.Copy(dst, src)
}

// bufConn reads through the bufio.Reader hijack handed back (it may already
// hold bytes the client pipelined behind the CONNECT/mitm'd request) and
// writes to the conn.
type bufConn struct {
	net.Conn
	r io.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
