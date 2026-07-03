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

func audit(aw io.Writer, host, port string, allowed bool) {
	// one json line per request -- the caller collects these as the egress log.
	b, _ := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "host": host, "port": port, "allowed": allowed,
	})
	aw.Write(append(b, '\n'))
}

// auditURL is audit + the full url -- only mitm'd requests have one.
func auditURL(aw io.Writer, host, port, url string, allowed bool) {
	b, _ := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "host": host, "port": port, "allowed": allowed, "url": url,
	})
	aw.Write(append(b, '\n'))
}

// Handler serves the proxy: CONNECT tunnels (mitm'd for hosts with url rules
// when s is non-nil), absolute-URI plain http otherwise. upstream is the
// RoundTripper for mitm'd requests (http.DefaultTransport in production).
// aw receives the audit (one json line per request); nil means os.Stdout --
// the sidecar shape, where `docker logs` is the collector.
func Handler(r Rule, s *Signer, upstream http.RoundTripper, aw io.Writer) http.HandlerFunc {
	if aw == nil {
		aw = os.Stdout
	}
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			handleConnect(w, req, r, s, upstream, aw)
			return
		}
		handleHTTP(w, req, r, aw)
	}
}

// handleConnect tunnels https. the target is req.Host (host:port). hosts with
// url rules are not tunneled blind -- their tls terminates here (mitm) so each
// request can be filtered by full url.
func handleConnect(w http.ResponseWriter, req *http.Request, r Rule, s *Signer, upstream http.RoundTripper, aw io.Writer) {
	host, port := splitHostPort(req.Host)
	if !r.permits(host) {
		audit(aw, host, port, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	if s != nil && r.mitmHost(host) {
		handleMitm(w, req, r, s, upstream, aw)
		return
	}
	audit(aw, host, port, true)

	remote, err := net.DialTimeout("tcp", req.Host, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	hj, ok := w.(http.Hijacker)
	if !ok {
		remote.Close()
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		remote.Close()
		return
	}
	go pipe(remote, client)
	go pipe(client, remote)
}

// handleHTTP forwards plain http (absolute-URI proxy requests, e.g. apt).
func handleHTTP(w http.ResponseWriter, req *http.Request, r Rule, aw io.Writer) {
	host, port := splitHostPort(req.Host)
	if port == "" {
		port = "80"
	}
	if !r.permits(host) {
		audit(aw, host, port, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	// plain http shows the path w/out mitm -- url rules apply directly, and the
	// audit gets the full url for free.
	hostPath := host + req.URL.Path
	if !r.permitsURL(hostPath) {
		auditURL(aw, host, port, hostPath, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	auditURL(aw, host, port, hostPath, true)

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
