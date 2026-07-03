// mitm: for hosts named by a url rule the proxy terminates tls with a leaf
// signed by the per-run CA, filters each request by full url, and re-dials the
// real host. everything else stays a blind tunnel (see proxy.go).
package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// URLRule is one host-anchored url pattern, e.g. "github.com/*/chisel-releases*".
// the host part (up to the first /) is a literal hostname; the rest is a glob
// where * matches any characters, / included.
type URLRule struct {
	host string
	re   *regexp.Regexp // matches host+path, e.g. "github.com/canonical/chisel-releases/x"
}

func ParseURLRules(pats []string) []URLRule {
	var out []URLRule
	for _, p := range pats {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		host, _, _ := strings.Cut(p, "/")
		// NOTE: wildcard/empty hosts are rejected at config validation; here we
		// just skip them so a hand-set env can't mitm the world by accident.
		if host == "" || strings.Contains(host, "*") {
			continue
		}
		var b strings.Builder
		b.WriteString("^")
		for _, part := range strings.Split(p, "*") {
			b.WriteString(regexp.QuoteMeta(part))
			b.WriteString(".*")
		}
		re := regexp.MustCompile(strings.TrimSuffix(b.String(), ".*") + "$")
		out = append(out, URLRule{host: host, re: re})
	}
	return out
}

// mitmHost says whether this host's tls must be terminated (it has url rules).
func (r Rule) mitmHost(host string) bool {
	host = strings.ToLower(host)
	for _, u := range r.DenyURLs {
		if u.host == host {
			return true
		}
	}
	for _, u := range r.AllowURLs {
		if u.host == host {
			return true
		}
	}
	return false
}

// permitsURL checks host+path (no scheme) against the url rules: a deny match
// always loses; then, if the host has allow-url rules, only matching urls pass.
// hosts with no url rules are unaffected.
func (r Rule) permitsURL(hostPath string) bool {
	hostPath = strings.ToLower(hostPath)
	for _, u := range r.DenyURLs {
		if u.re.MatchString(hostPath) {
			return false
		}
	}
	host, _, _ := strings.Cut(hostPath, "/")
	restricted, matched := false, false
	for _, u := range r.AllowURLs {
		if u.host != host {
			continue
		}
		restricted = true
		if u.re.MatchString(hostPath) {
			matched = true
			break
		}
	}
	return !restricted || matched
}

// Signer mints per-host leaf certs signed by the run CA, cached.
type Signer struct {
	ca    tls.Certificate
	caX   *x509.Certificate
	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// NewSigner accepts the CA cert/key as either a file path or inline PEM
// (detected by the PEM header). inline keeps the key out of any mounted or
// image path, so the proxy can run as a non-root user with no fs access.
func NewSigner(certSpec, keySpec string) (*Signer, error) {
	ca, err := loadKeyPair(certSpec, keySpec)
	if err != nil {
		return nil, err
	}
	caX, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &Signer{ca: ca, caX: caX, cache: map[string]*tls.Certificate{}}, nil
}

func loadKeyPair(certSpec, keySpec string) (tls.Certificate, error) {
	if strings.Contains(certSpec, "-----BEGIN") || strings.Contains(keySpec, "-----BEGIN") {
		return tls.X509KeyPair([]byte(certSpec), []byte(keySpec))
	}
	return tls.LoadX509KeyPair(certSpec, keySpec)
}

func (s *Signer) leaf(host string) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caX, &key.PublicKey, s.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	s.cache[host] = c
	return c, nil
}

// handleMitm terminates the CONNECT tls with a signed leaf, then serves the
// decrypted requests, filtering each by url and forwarding the allowed ones to
// the real host via upstream.
func handleMitm(w http.ResponseWriter, req *http.Request, r Rule, s *Signer, upstream http.RoundTripper) {
	host, port := splitHostPort(req.Host)
	if port == "" {
		port = "443"
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	raw, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer raw.Close()

	tconn := tls.Server(raw, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if chi.ServerName != "" {
				return s.leaf(chi.ServerName)
			}
			return s.leaf(host)
		},
		NextProtos: []string{"http/1.1"}, // NOTE: no h2; clients downgrade fine
	})
	defer tconn.Close()
	if err := tconn.Handshake(); err != nil {
		return
	}

	br := bufio.NewReader(tconn)
	for {
		inner, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		hostPath := host + inner.URL.Path
		if !r.permitsURL(hostPath) {
			auditURL(host, port, hostPath, false)
			resp := &http.Response{StatusCode: http.StatusForbidden, ProtoMajor: 1, ProtoMinor: 1,
				Body: http.NoBody, Header: http.Header{}, Close: true}
			resp.Write(tconn)
			return
		}
		auditURL(host, port, hostPath, true)

		inner.URL.Scheme = "https"
		inner.URL.Host = req.Host
		inner.RequestURI = ""
		resp, err := upstream.RoundTrip(inner)
		if err != nil {
			(&http.Response{StatusCode: http.StatusBadGateway, ProtoMajor: 1, ProtoMinor: 1,
				Body: http.NoBody, Header: http.Header{}, Close: true}).Write(tconn)
			return
		}
		err = resp.Write(tconn)
		resp.Body.Close()
		if err != nil || resp.Close || inner.Close {
			return
		}
	}
}
