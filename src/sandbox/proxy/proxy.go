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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
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
	// Idle is how long a terminated connection may sit between requests.
	// zero: idleWait.
	Idle time.Duration

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
// zero linger so the peer sees a reset rather than a clean close. the close
// is of the transport under any tls, which would say goodbye first.
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
	for {
		under, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		conn = under.NetConn()
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
	v := p.Rule.Decide(host, port, req.Method, req.URL.Path, req.URL.RawQuery)
	ev := Event{Ev: "req", Host: host, Port: portStr(port), Kind: "http", Rule: v.Rule, Reason: v.Reason,
		Method: req.Method, URL: auditURL(host, req.URL), UA: req.UserAgent()}
	if !v.Allowed {
		ev.Status = p.refuse(w)
		p.audit(ev)
		return
	}
	p.forward(w, req, ev, nil)
}

// auditURL is a request's url as the audit records it: the host the policy
// saw, the path and the query.
func auditURL(host string, u *url.URL) string {
	if u.RawQuery != "" {
		return host + u.Path + "?" + u.RawQuery
	}
	return host + u.Path
}

// errChainAuth is the upstream proxy asking for a credential it was given,
// or was not: its failure, not the origin's answer.
var errChainAuth = errors.New("upstream proxy: 407")

// errSwitched is an origin switching protocols, which it was not asked to.
var errSwitched = errors.New("the origin switched protocols")

// forwarded are the headers a client may say its own forwarding in. the
// reverse proxy drops them for a rewrite to set anew; this proxy is not a
// hop the origin is told of, and passes them as sent.
var forwarded = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// forward sends an admitted request to its origin, relays the answer and
// audits the exchange as ev. origin is where a request read off a terminated
// connection goes; nil is plain http, whose request names its own. the relay
// is net/http's reverse proxy: hop-by-hop headers stay on their hop, a
// stream is flushed as it comes, and a request body nobody read is the
// serving connection's to settle.
func (p *Proxy) forward(w http.ResponseWriter, req *http.Request, ev Event, origin *url.URL) {
	start := time.Now()
	var sent, got countReader
	defer func() {
		ev.In, ev.Out, ev.MS = got.n.Load(), sent.n.Load(), time.Since(start).Milliseconds()
		p.audit(ev)
	}()
	(&httputil.ReverseProxy{
		Transport:  p.Upstream,
		BufferPool: copyBufs,
		ErrorLog:   log.New(io.Discard, "", 0), // a failed exchange is the audit's to tell
		Rewrite: func(pr *httputil.ProxyRequest) {
			if origin != nil {
				pr.Out.URL.Scheme, pr.Out.URL.Host = origin.Scheme, origin.Host
			}
			// the request goes on as it was decided on: the reverse proxy
			// re-encodes a query it cannot parse.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Trailer = pr.In.Trailer
			for _, h := range forwarded {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
			// a switch of protocols (a websocket) is not offered to the
			// origin: what followed it would pass uninspected.
			pr.Out.Header.Del("Upgrade")
			pr.Out.Header.Del("Connection")
			if pr.Out.Body != nil {
				sent.r, pr.Out.Body = pr.Out.Body, &sent
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if origin == nil && resp.StatusCode == http.StatusProxyAuthRequired && p.Chain.For("http", req.URL.Host) != nil {
				return errChainAuth
			}
			// what cannot be relayed as an answer: a switch nobody asked
			// for, whose body is the origin's connection, and a status
			// the server has no line for.
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errSwitched
			}
			if resp.StatusCode < 100 {
				return fmt.Errorf("the origin answered with status %d", resp.StatusCode)
			}
			ev.Allowed, ev.Status = true, resp.StatusCode
			if origin != nil {
				ev.Revocation = "unchecked"
				if resp.TLS != nil {
					ev.Revocation = stapleStatus(*resp.TLS)
				}
			}
			// the answer is the origin's: the server would add a date and
			// guess a content type where it sent none.
			for _, h := range []string{"Content-Type", "Date"} {
				if _, ok := resp.Header[h]; !ok {
					w.Header()[h] = nil
				}
			}
			// count what came from upstream, not what reached the client: a
			// client that hangs up early still cost the upstream bytes
			// already read.
			got.r, resp.Body = resp.Body, &got
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			ev.Allowed, ev.Status = false, http.StatusBadGateway
			switch {
			case errors.Is(err, errChainAuth):
				ev.Reason = "upstream: 407"
			case errors.Is(err, errSwitched):
				ev.Reason = "upgrade"
			case req.Context().Err() != nil:
				// the client hung up on a request it was allowed: not a
				// refusal, and no failure of the way out.
				ev.Allowed, ev.Status, ev.Reason = true, 0, "client-gone"
			default:
				ev.Reason = upstreamReason(err)
			}
			// the reason, not the error: that may name an address a
			// sandbox has no way to learn.
			http.Error(w, "egress: "+ev.Reason, http.StatusBadGateway)
		},
	}).ServeHTTP(w, req)
}

// copyBufs lends the reverse proxy the buffer it copies an answer through,
// which it would otherwise make anew for each.
var copyBufs = &bufPool{sync.Pool{New: func() any { return new([32 << 10]byte) }}}

type bufPool struct{ sync.Pool }

func (p *bufPool) Get() []byte  { return p.Pool.Get().(*[32 << 10]byte)[:] }
func (p *bufPool) Put(b []byte) { p.Pool.Put((*[32 << 10]byte)(b)) }

// countReader counts the bytes read through it. the count is read while a
// transport may still be sending the request body it wraps.
type countReader struct {
	r io.ReadCloser
	n atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
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

// NetConn is the connection under the reader, as a tls.Conn names its own.
func (c bufConn) NetConn() net.Conn { return c.Conn }
