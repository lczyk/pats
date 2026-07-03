// Command egress-proxy is pats' sandbox egress filter: a tiny forward proxy
// (CONNECT for https, absolute-URI for http) that allows/denies by host and
// logs every request as one json line to stdout (the egress audit). the
// engine lives in src/sandbox/proxy; this binary is the env-configured shell.
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
	"net/http"
	"os"
	"strings"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

func main() {
	addr := envOr("PROXY_ADDR", ":8080")
	r := proxy.Rule{
		DefaultAllow: envOr("PROXY_DEFAULT", "deny") == "allow",
		Allow:        splitList(os.Getenv("PROXY_ALLOW")),
		Deny:         splitList(os.Getenv("PROXY_DENY")),
		DenyURLs:     proxy.ParseURLRules(splitList(os.Getenv("PROXY_DENY_URLS"))),
		AllowURLs:    proxy.ParseURLRules(splitList(os.Getenv("PROXY_ALLOW_URLS"))),
	}
	var s *proxy.Signer
	if len(r.DenyURLs)+len(r.AllowURLs) > 0 {
		var err error
		if s, err = proxy.NewSigner(os.Getenv("PROXY_CA_CERT"), os.Getenv("PROXY_CA_KEY")); err != nil {
			panic("url rules set but CA unusable: " + err.Error())
		}
	}

	srv := &http.Server{Addr: addr, Handler: proxy.Handler(r, s, http.DefaultTransport, os.Stdout)}
	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}

// splitList splits a comma-separated env value, dropping empties.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
