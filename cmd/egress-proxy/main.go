// Command egress-proxy is pats' sandbox egress filter: a tiny forward proxy
// (CONNECT for https, absolute-URI for http) that allows/denies by host and
// logs every request as one json line to stdout (the egress audit).
//
// config via env:
//
//	PROXY_ADDR      listen address (default :8080)
//	PROXY_DEFAULT   "deny" (allowlist) or "allow" (denylist); default "deny"
//	PROXY_ALLOW     comma-separated hosts reachable when default=deny
//	PROXY_DENY      comma-separated hosts blocked when default=allow
//	PROXY_DENY_URLS  comma-separated host-anchored url patterns (mitm mode);
//	                 "*" matches anything, "/" included, e.g.
//	                 "github.com/*/chisel-releases*". hosts named here get their
//	                 tls terminated with a leaf signed by the run CA.
//	PROXY_ALLOW_URLS -//-; a host with allow rules only passes matching urls
//	                 (deny rules win). hosts named here are mitm'd too.
//	PROXY_CA_CERT    run CA cert: a pem file path, or the pem itself inline;
//	                 required with url rules
//	PROXY_CA_KEY     run CA key;  -//-
//
// host entries match exactly, or as a suffix when written ".example.com" or
// "*.example.com" (so ".ubuntu.com" covers archive.ubuntu.com).
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type rule struct {
	def       bool     // true = default allow
	allow     []string // exceptions to deny-default
	deny      []string // exceptions to allow-default
	denyURLs  []urlRule
	allowURLs []urlRule // per-host allowlist: a host with rules only passes matching urls
}

func (r rule) permits(host string) bool {
	if r.def { // default allow -> blocked iff in deny
		return !matchAny(host, r.deny)
	}
	return matchAny(host, r.allow) // default deny -> allowed iff in allow
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

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func audit(host, port string, allowed bool) {
	// one json line per request -- pats collects these as the run's egress log.
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

func main() {
	addr := envOr("PROXY_ADDR", ":8080")
	r := rule{
		def:       envOr("PROXY_DEFAULT", "deny") == "allow",
		allow:     splitList(os.Getenv("PROXY_ALLOW")),
		deny:      splitList(os.Getenv("PROXY_DENY")),
		denyURLs:  parseURLRules(splitList(os.Getenv("PROXY_DENY_URLS"))),
		allowURLs: parseURLRules(splitList(os.Getenv("PROXY_ALLOW_URLS"))),
	}
	var s *signer
	if len(r.denyURLs)+len(r.allowURLs) > 0 {
		var err error
		if s, err = newSigner(os.Getenv("PROXY_CA_CERT"), os.Getenv("PROXY_CA_KEY")); err != nil {
			panic("url rules set but CA unusable: " + err.Error())
		}
	}

	srv := &http.Server{Addr: addr, Handler: handler(r, s, http.DefaultTransport)}
	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func handler(r rule, s *signer, upstream http.RoundTripper) http.HandlerFunc {
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
func handleConnect(w http.ResponseWriter, req *http.Request, r rule, s *signer, upstream http.RoundTripper) {
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
func handleHTTP(w http.ResponseWriter, req *http.Request, r rule) {
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
	// audit gets the full url for free.
	hostPath := host + req.URL.Path
	if !r.permitsURL(hostPath) {
		auditURL(host, port, hostPath, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	auditURL(host, port, hostPath, true)

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
