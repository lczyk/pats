package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Chain is the machine's own proxy, which the proxy's upstream connections go
// through: tunnels and tls inspection by CONNECT to the https upstream, plain
// http in absolute form to the http one. a destination NoProxy names, or one
// whose scheme has no upstream, is dialled directly.
type Chain struct {
	HTTPS, HTTP *url.URL // nil: that scheme goes direct
	NoProxy     string
	match       func(*url.URL) (*url.URL, error)
}

// the keys pats hands its own proxy the upstream under, in place of the
// machine's proxy env, which the sandbox's cli or image may set or rewrite.
const (
	EnvUpstreamHTTPS = "PATS_EGRESS_UPSTREAM_HTTPS"
	EnvUpstreamHTTP  = "PATS_EGRESS_UPSTREAM_HTTP"
	EnvNoProxy       = "PATS_EGRESS_NO_PROXY"
)

// ChainFromEnv reads the machine's proxy the way go's net/http does:
// HTTPS_PROXY, HTTP_PROXY, NO_PROXY, uppercase first, lowercase as fallback.
// ALL_PROXY is not read. when any of pats's own keys is set, they are read
// instead. nil, nil: no upstream.
func ChainFromEnv(getenv func(string) string) (*Chain, error) {
	var c httpproxy.Config
	if getenv(EnvUpstreamHTTPS) != "" || getenv(EnvUpstreamHTTP) != "" {
		c = httpproxy.Config{HTTPSProxy: getenv(EnvUpstreamHTTPS), HTTPProxy: getenv(EnvUpstreamHTTP), NoProxy: getenv(EnvNoProxy)}
	} else {
		pick := func(k string) string {
			if v := getenv(k); v != "" {
				return v
			}
			return getenv(strings.ToLower(k))
		}
		c = httpproxy.Config{HTTPSProxy: pick("HTTPS_PROXY"), HTTPProxy: pick("HTTP_PROXY"), NoProxy: pick("NO_PROXY")}
	}
	return NewChain(c.HTTPSProxy, c.HTTPProxy, c.NoProxy)
}

// NewChain builds a Chain from the three values. an upstream is
// http://[user:pass@]host:port; a value with no scheme is taken as http, as
// net/http takes it. any other scheme is an error, never a direct dial.
func NewChain(https, http, noProxy string) (*Chain, error) {
	if https == "" && http == "" {
		return nil, nil
	}
	c := &Chain{NoProxy: noProxy}
	var err error
	if c.HTTPS, err = parseUpstream("HTTPS_PROXY", https); err != nil {
		return nil, err
	}
	if c.HTTP, err = parseUpstream("HTTP_PROXY", http); err != nil {
		return nil, err
	}
	str := func(u *url.URL) string {
		if u == nil {
			return ""
		}
		return u.String()
	}
	c.match = (&httpproxy.Config{HTTPSProxy: str(c.HTTPS), HTTPProxy: str(c.HTTP), NoProxy: noProxy}).ProxyFunc()
	return c, nil
}

func parseUpstream(key, v string) (*url.URL, error) {
	if v == "" {
		return nil, nil
	}
	// net/http takes a scheme-less value as http://. the value is never
	// echoed back: it may hold a credential in a place Redacted cannot see.
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s: not a proxy url (want http://[user:pass@]host:port)", key)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("%s: the upstream is a %s:// proxy; pats chains through an http:// proxy only",
			key, u.Scheme)
	}
	u.Path = ""
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "80")
	}
	return u, nil
}

// Redact is u with its password masked, for every message naming an upstream.
func Redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Redacted()
}

// Describe is the start line's upstream: the https one, and the http one when
// it differs. empty for a nil chain.
func (c *Chain) Describe() string {
	if c == nil {
		return ""
	}
	switch {
	case c.HTTPS == nil:
		return Redact(c.HTTP) + " (http only)"
	case c.HTTP == nil:
		return Redact(c.HTTPS) + " (https only)"
	case c.HTTPS.String() == c.HTTP.String():
		return Redact(c.HTTPS)
	}
	return Redact(c.HTTPS) + ", http via " + Redact(c.HTTP)
}

// For is the upstream a request to scheme://host:port goes through, nil when
// it goes direct.
func (c *Chain) For(scheme, hostport string) *url.URL {
	if c == nil {
		return nil
	}
	u, err := c.match(&url.URL{Scheme: scheme, Host: hostport})
	if err != nil || u == nil {
		return nil
	}
	if scheme == "http" {
		return c.HTTP
	}
	return c.HTTPS
}

// Upstreams lists the distinct upstream urls.
func (c *Chain) Upstreams() []*url.URL {
	if c == nil {
		return nil
	}
	var out []*url.URL
	for _, u := range []*url.URL{c.HTTPS, c.HTTP} {
		if u != nil && (len(out) == 0 || out[0].Host != u.Host) {
			out = append(out, u)
		}
	}
	return out
}

// Env is the proxy env pats hands its own proxy: its keys, never the
// machine's names, so a cli or image setting those cannot change the route.
func (c *Chain) Env() map[string]string {
	if c == nil {
		return nil
	}
	env := map[string]string{EnvNoProxy: c.NoProxy}
	if c.HTTPS != nil {
		env[EnvUpstreamHTTPS] = c.HTTPS.String()
	}
	if c.HTTP != nil {
		env[EnvUpstreamHTTP] = c.HTTP.String()
	}
	return env
}

// AgentEnv is the machine's proxy env as an open sandbox's agent gets it.
func (c *Chain) AgentEnv() map[string]string {
	if c == nil {
		return nil
	}
	env := map[string]string{"NO_PROXY": c.NoProxy, "no_proxy": c.NoProxy}
	if c.HTTPS != nil {
		env["HTTPS_PROXY"], env["https_proxy"] = c.HTTPS.String(), c.HTTPS.String()
	}
	if c.HTTP != nil {
		env["HTTP_PROXY"], env["http_proxy"] = c.HTTP.String(), c.HTTP.String()
	}
	return env
}

// Loopback is the first upstream on the host's own loopback, nil if none.
func (c *Chain) Loopback() *url.URL {
	for _, u := range c.Upstreams() {
		h := u.Hostname()
		if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
			return u
		}
		if a, err := netip.ParseAddr(h); err == nil && a.Unmap().IsLoopback() {
			return u
		}
	}
	return nil
}

// UpstreamError is a destination the upstream did not connect: its CONNECT
// answered with a non-2xx status, or the upstream itself was unreachable
// (Status "dial").
type UpstreamError struct {
	Upstream string // redacted
	Status   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %s: %s", e.Upstream, e.Status)
}

// upstreamTimeout bounds the CONNECT exchange with the upstream.
const upstreamTimeout = 30 * time.Second

// dialUpstream connects to u itself. the upstream is exempt from the guard:
// a proxy on the lan is the point.
func dialUpstream(ctx context.Context, u *url.URL) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: upstreamTimeout}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, &UpstreamError{Upstream: Redact(u), Status: "dial"}
	}
	return conn, nil
}

// connectVia opens a tunnel to addr through the upstream u by CONNECT, with
// basic auth when u carries a user. the conn it returns is as if addr had
// been dialled directly.
func connectVia(ctx context.Context, u *url.URL, addr string) (net.Conn, error) {
	conn, err := dialUpstream(ctx, u)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(upstreamTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if auth := basicAuth(u); auth != "" {
		req.Header.Set("Proxy-Authorization", auth)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, &UpstreamError{Upstream: Redact(u), Status: "dial"}
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, &UpstreamError{Upstream: Redact(u), Status: "dial"}
	}
	// a 2xx to CONNECT has no body whatever its headers say: what follows is
	// the tunnel, so the body is never read or closed.
	if resp.StatusCode/100 != 2 {
		conn.Close()
		return nil, &UpstreamError{Upstream: Redact(u), Status: fmt.Sprint(resp.StatusCode)}
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return bufConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

func basicAuth(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
}

// Reach dials every upstream's host:port until it connects or timeout ends:
// a route, not the credential or the upstream's policy. it retries, since a
// sidecar's way out may be attached just after it starts.
func (c *Chain) Reach(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, u := range c.Upstreams() {
		for {
			left := time.Until(deadline)
			if left <= 0 {
				left = time.Millisecond
			}
			conn, err := (&net.Dialer{Timeout: left}).DialContext(ctx, "tcp", u.Host)
			if err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("upstream %s unreachable: %w", Redact(u), unwrapOp(err))
			}
			select {
			case <-ctx.Done():
			case <-time.After(reachRetry):
			}
		}
	}
	return nil
}

// reachRetry is the pause between Reach's dials.
const reachRetry = 250 * time.Millisecond

// unwrapOp drops net.OpError's "dial tcp a:b:" prefix, which repeats the
// upstream already named.
func unwrapOp(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}

// chainedDial is the proxy's dial behind c: a destination the https upstream
// covers goes by CONNECT, after the guard checks a literal address (a name is
// resolved by the upstream, so its address is the upstream's to check); the
// rest is direct, guarded.
func chainedDial(c *Chain, g *Guard, direct func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		u := c.For("https", addr)
		if u == nil {
			return direct(ctx, network, addr)
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if a, err := netip.ParseAddr(host); err == nil {
			if err := g.check(a); err != nil {
				return nil, err
			}
		}
		return connectVia(ctx, u, addr)
	}
}

// chainRouter routes the proxy's round trips behind a chain: https (tls
// inspection) over the transport whose dial is chainedDial, plain http to the
// http upstream in absolute form when it covers the host, else direct.
type chainRouter struct {
	chain                  *Chain
	guard                  *Guard
	https, direct, proxied http.RoundTripper
}

func (r chainRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" {
		return r.https.RoundTrip(req)
	}
	if r.chain.For("http", req.URL.Host) == nil {
		return r.direct.RoundTrip(req)
	}
	if a, err := netip.ParseAddr(req.URL.Hostname()); err == nil {
		if err := r.guard.check(a); err != nil {
			return nil, err
		}
	}
	return r.proxied.RoundTrip(req)
}
