// Command egress-proxy is pats' sandbox egress filter: a tiny forward proxy
// (CONNECT for https, absolute-URI for http) that allows/denies by host and
// logs every request as one json line to stdout (the egress audit).
//
// config via env:
//
//	PROXY_ADDR     listen address (default :8080)
//	PROXY_DEFAULT  "deny" (allowlist) or "allow" (denylist); default "deny"
//	PROXY_ALLOW    comma-separated hosts reachable when default=deny
//	PROXY_DENY     comma-separated hosts blocked when default=allow
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
	def   bool     // true = default allow
	allow []string // exceptions to deny-default
	deny  []string // exceptions to allow-default
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

func main() {
	addr := envOr("PROXY_ADDR", ":8080")
	r := rule{
		def:   envOr("PROXY_DEFAULT", "deny") == "allow",
		allow: splitList(os.Getenv("PROXY_ALLOW")),
		deny:  splitList(os.Getenv("PROXY_DENY")),
	}

	srv := &http.Server{Addr: addr, Handler: handler(r)}
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

func handler(r rule) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			handleConnect(w, req, r)
			return
		}
		handleHTTP(w, req, r)
	}
}

// handleConnect tunnels https. the target is req.Host (host:port).
func handleConnect(w http.ResponseWriter, req *http.Request, r rule) {
	host, port := splitHostPort(req.Host)
	if !r.permits(host) {
		audit(host, port, false)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	audit(host, port, true)

	upstream, err := net.DialTimeout("tcp", req.Host, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
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
	audit(host, port, true)

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
