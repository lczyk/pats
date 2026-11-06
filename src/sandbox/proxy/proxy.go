// Package proxy is a filtering forward proxy for sandboxed processes: CONNECT
// for https, absolute-URI for http, allow/deny by host, and (for hosts named
// by a url entry) tls termination with a per-run CA so requests are filtered
// by full url. every request is audited as one json line. the policy grammar
// is in rule.go; Serve (serve.go) is the entrypoint pats and the sidecar use.
package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Proxy serves one policy. zero fields take the production defaults: a guarded
// dialer, a transport built from it, 403 on refusal, audit to io.Discard.
type Proxy struct {
	Rule   Rule
	Signer *Signer // nil: no mitm (url entries then only apply to plain http)
	// Upstream handles mitm'd and plain-http requests. nil: a transport over
	// Dial, verifying against Roots.
	Upstream http.RoundTripper
	// Dial opens every upstream connection (tunnels included). nil: Dialer().
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DenyResponse is what a refused request sees: "403" (default), "404",
	// "reset" (the connection is reset), or "timeout" (held, then reset).
	DenyResponse string
	Audit        io.Writer

	once sync.Once
	mu   sync.Mutex
}

// Event is one audit line.
type Event struct {
	TS      string `json:"ts"`
	Host    string `json:"host"`
	Port    string `json:"port,omitempty"`
	Allowed bool   `json:"allowed"`
	URL     string `json:"url,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (p *Proxy) init() {
	p.once.Do(func() {
		if p.Dial == nil {
			p.Dial = Dialer(nil).DialContext
		}
		if p.Upstream == nil {
			p.Upstream = &http.Transport{DialContext: p.Dial, ForceAttemptHTTP2: true}
		}
		if p.Audit == nil {
			p.Audit = io.Discard
		}
	})
}

func (p *Proxy) audit(ev Event) {
	ev.TS = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(ev)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Audit.Write(append(b, '\n'))
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p.init()
	if req.Method == http.MethodConnect {
		p.handleConnect(w, req)
		return
	}
	p.handleHTTP(w, req)
}

// denyHold is how long a "timeout" refusal holds the connection before the
// reset. the server's idle timeout does not cover a handler, so this is the
// only bound on it.
const denyHold = 60 * time.Second

// refuse answers a refused request per DenyResponse. after it the connection
// is unusable by design.
func (p *Proxy) refuse(w http.ResponseWriter) {
	switch p.DenyResponse {
	case "404":
		http.Error(w, "not found", http.StatusNotFound)
	case "reset":
		resetConn(w, 0)
	case "timeout":
		resetConn(w, denyHold)
	default:
		http.Error(w, "egress denied", http.StatusForbidden)
	}
}

// resetConn hijacks the connection, holds it for d, and closes it with a
// zero linger so the peer sees a reset rather than a clean close.
func resetConn(w http.ResponseWriter, d time.Duration) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if d > 0 {
		time.Sleep(d)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	conn.Close()
}

// handleConnect tunnels https. hosts named by a url entry are not tunneled
// blind: their tls terminates here (mitm) so each request is filtered by url.
// everything else is piped as-is once the ClientHello names the CONNECT host.
func (p *Proxy) handleConnect(w http.ResponseWriter, req *http.Request) {
	host, port := splitHostPort(req.Host, 443)
	v := p.Rule.DecideHost(host, port)
	if !v.Allowed {
		p.audit(Event{Host: host, Port: portStr(port), Rule: v.Rule, Reason: v.Reason})
		p.refuse(w)
		return
	}
	if p.Signer != nil && p.Rule.MitmHost(host, port) {
		p.handleMitm(w, req, host, port)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	client, crw, err := hj.Hijack()
	if err != nil {
		return
	}
	// the 200 must go straight to the wire: net/http's normal WriteHeader path
	// flushes a full response (Date, Transfer-Encoding: chunked, ...) once
	// anything is written after hijack, and RFC 9110 9.3.6 forbids framing
	// headers on a 2xx CONNECT response -- strict parsers (rust hyper) reject
	// the tunnel over it.
	if _, err := client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		client.Close()
		return
	}
	// crw.Reader may already hold bytes the client pipelined behind the
	// CONNECT request (e.g. a TLS ClientHello sent without waiting for the
	// 200) -- read through it, or those bytes are silently dropped and the
	// tunnel stalls.
	cconn := bufConn{Conn: client, r: crw.Reader}

	hello, reason := p.peekHello(cconn, host, port)
	if reason != "" {
		p.audit(Event{Host: host, Port: portStr(port), Rule: v.Rule, Reason: reason})
		client.Close()
		return
	}
	remote, err := p.Dial(req.Context(), "tcp", net.JoinHostPort(host, portStr(port)))
	if err != nil {
		p.audit(Event{Host: host, Port: portStr(port), Rule: v.Rule, Reason: dialReason(err)})
		client.Close()
		return
	}
	p.audit(Event{Host: host, Port: portStr(port), Allowed: true, Rule: v.Rule})
	if len(hello) > 0 {
		if _, err := remote.Write(hello); err != nil {
			client.Close()
			remote.Close()
			return
		}
	}
	go pipe(remote, cconn)
	go pipe(client, remote)
}

// handleHTTP forwards plain http (absolute-URI proxy requests, e.g. apt). the
// path is visible without mitm, so url entries apply directly.
func (p *Proxy) handleHTTP(w http.ResponseWriter, req *http.Request) {
	host, port := splitHostPort(req.Host, 80)
	fullURL := host + req.URL.Path
	if req.URL.RawQuery != "" {
		fullURL += "?" + req.URL.RawQuery
	}
	v := p.Rule.Decide(host, port, req.Method, req.URL.Path, req.URL.RawQuery)
	if !v.Allowed {
		p.audit(Event{Host: host, Port: portStr(port), URL: fullURL, Rule: v.Rule, Reason: v.Reason})
		p.refuse(w)
		return
	}
	p.audit(Event{Host: host, Port: portStr(port), Allowed: true, URL: fullURL, Rule: v.Rule})

	req.RequestURI = ""
	resp, err := p.Upstream.RoundTrip(req)
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

// splitHostPort splits "host[:port]", defaulting the port.
func splitHostPort(hp string, def uint16) (string, uint16) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return hp, def
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return h, def
	}
	return h, uint16(n)
}

func portStr(p uint16) string { return strconv.Itoa(int(p)) }

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
	r *bufio.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
