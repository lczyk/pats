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
	"errors"
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
	// Chain is the upstream proxy, when there is one: a 407 it answers a
	// plain http request with is its failure, not the origin's answer.
	Chain *Chain

	once sync.Once
	mu   sync.Mutex
}

// Event is one audit line. Ev says which of three:
//
//	open  -- a CONNECT: kind tunnel (tls) or raw, the verdict, the address
//	         dialled. a refused CONNECT is an open with allowed false.
//	close -- a tunnel ended: bytes each way and its duration. never a
//	         refusal (allowed is always true).
//	req   -- a request the proxy saw in clear: kind mitm or http, method,
//	         url, user agent, upstream status, body bytes each way.
//
// Rule is the entry that decided, or "default". Reason says why a refusal or
// failure happened.
type Event struct {
	TS         string `json:"ts"`
	Ev         string `json:"ev"`
	Host       string `json:"host"`
	Port       string `json:"port,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Allowed    bool   `json:"allowed"`
	Rule       string `json:"rule,omitempty"`
	Reason     string `json:"reason,omitempty"`
	IP         string `json:"ip,omitempty"`
	Method     string `json:"method,omitempty"`
	URL        string `json:"url,omitempty"`
	UA         string `json:"ua,omitempty"`
	Status     int    `json:"status,omitempty"`
	In         int64  `json:"in,omitempty"`  // bytes received from upstream
	Out        int64  `json:"out,omitempty"` // bytes sent upstream
	MS         int64  `json:"ms,omitempty"`
	Revocation string `json:"revocation,omitempty"` // mitm: good | unchecked
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

// refuse answers a refused request per DenyResponse and returns the status it
// sent (0 when the connection was reset). after it the connection is
// unusable by design.
func (p *Proxy) refuse(w http.ResponseWriter) int {
	switch p.DenyResponse {
	case "404":
		http.Error(w, "not found", http.StatusNotFound)
		return http.StatusNotFound
	case "reset":
		resetConn(w, 0)
		return 0
	case "timeout":
		resetConn(w, denyHold)
		return 0
	default:
		http.Error(w, "egress denied", http.StatusForbidden)
		return http.StatusForbidden
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
	open := Event{Ev: "open", Host: host, Port: portStr(port), Kind: "tunnel", Rule: v.Rule, Reason: v.Reason}
	if !v.Allowed {
		open.Status = p.refuse(w)
		p.audit(open)
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
	if hello == nil && reason == "" {
		open.Kind = "raw"
	}
	if reason != "" {
		open.Reason = reason
		p.audit(open)
		client.Close()
		return
	}
	start := time.Now()
	remote, err := p.Dial(req.Context(), "tcp", net.JoinHostPort(host, portStr(port)))
	if err != nil {
		open.Reason = dialReason(err)
		p.audit(open)
		client.Close()
		return
	}
	open.Allowed = true
	if ra, ok := remote.RemoteAddr().(*net.TCPAddr); ok {
		open.IP = ra.IP.String()
	}
	p.audit(open)
	var out int64
	closeEv := func(in int64) {
		p.audit(Event{Ev: "close", Host: host, Port: portStr(port), Kind: open.Kind, Allowed: true,
			In: in, Out: out, MS: time.Since(start).Milliseconds()})
	}
	if len(hello) > 0 {
		n, err := remote.Write(hello)
		out += int64(n)
		if err != nil {
			client.Close()
			remote.Close()
			closeEv(0)
			return
		}
	}
	go func() {
		var wg sync.WaitGroup
		var in int64
		wg.Add(2)
		go func() { defer wg.Done(); out += pipe(remote, cconn) }()
		go func() { defer wg.Done(); in = pipe(client, remote) }()
		wg.Wait()
		closeEv(in)
	}()
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
	ev := Event{Ev: "req", Host: host, Port: portStr(port), Kind: "http", Rule: v.Rule, Reason: v.Reason,
		Method: req.Method, URL: fullURL, UA: req.UserAgent()}
	if !v.Allowed {
		ev.Status = p.refuse(w)
		p.audit(ev)
		return
	}

	start := time.Now()
	body := countBody(req)
	req.RequestURI = ""
	resp, err := p.Upstream.RoundTrip(req)
	if err != nil {
		ev.Reason, ev.Status = upstreamReason(err), http.StatusBadGateway
		ev.Out, ev.MS = body.n, time.Since(start).Milliseconds()
		p.audit(ev)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired && p.Chain.For("http", req.URL.Host) != nil {
		ev.Reason, ev.Status = "upstream: 407", http.StatusBadGateway
		ev.Out, ev.MS = body.n, time.Since(start).Milliseconds()
		p.audit(ev)
		http.Error(w, "upstream proxy: 407", http.StatusBadGateway)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// count what came from upstream, not what reached the client: a client
	// that hangs up early still cost the upstream bytes already read.
	in := &countReader{r: resp.Body}
	_, _ = io.Copy(w, in)
	ev.Allowed, ev.Status, ev.In, ev.Out, ev.MS = true, resp.StatusCode, in.n, body.n, time.Since(start).Milliseconds()
	p.audit(ev)
}

// countBody wraps req's body to count it. a request without one keeps its nil
// or NoBody: a non-empty Body of unknown length would turn a plain GET into a
// chunked upload on the way out.
func countBody(req *http.Request) *countReader {
	c := &countReader{r: req.Body}
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
		req.Body = c
	}
	return c
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.ReadCloser
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) Close() error { return c.r.Close() }

// upstreamReason names a failed upstream round trip for the audit: a revoked
// certificate, a dial the guard refused, or any other failure to reach it.
func upstreamReason(err error) string {
	if errors.Is(err, ErrRevoked) {
		return "revoked"
	}
	return dialReason(err)
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

// pipe copies src to dst until either side ends, then closes both, and
// returns the bytes copied.
func pipe(dst, src net.Conn) int64 {
	defer dst.Close()
	defer src.Close()
	n, _ := io.Copy(dst, src)
	return n
}

// bufConn reads through the bufio.Reader hijack handed back (it may already
// hold bytes the client pipelined behind the CONNECT/mitm'd request) and
// writes to the conn.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
