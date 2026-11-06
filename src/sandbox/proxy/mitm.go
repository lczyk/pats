// mitm: for hosts named by a url entry the proxy terminates tls with a leaf
// signed by the per-run CA, filters each request by full url, and re-dials the
// real host. everything else stays a tunnel (see proxy.go).
package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

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

var errSNIMismatch = errors.New("sni does not name the CONNECT host")

// handleMitm terminates the CONNECT tls with a leaf for the CONNECT host, then
// serves the decrypted requests, filtering each by url and forwarding the
// allowed ones to the real host via Upstream. the leaf is only ever for the
// CONNECT host and each inner request's Host must name it too: a shared
// front end must not be reached under a name the policy never saw.
func (p *Proxy) handleMitm(w http.ResponseWriter, req *http.Request, host string, port uint16) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	raw, rrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer raw.Close()
	// the 200 must go straight to the wire, not through WriteHeader -- see the
	// matching comment in handleConnect (proxy.go).
	if _, err := raw.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		return
	}

	want, _ := NormalizeHost(host)
	// rrw.Reader may already hold bytes the client pipelined behind the
	// CONNECT (e.g. the TLS ClientHello) -- read through it, or the handshake
	// below stalls waiting for bytes that already arrived.
	tconn := tls.Server(bufConn{Conn: raw, r: rrw.Reader}, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if chi.ServerName != "" {
				if got, err := NormalizeHost(chi.ServerName); err != nil || got != want {
					p.audit(Event{Ev: "open", Host: host, Port: portStr(port), Kind: "tunnel", Rule: "mitm", Reason: "sni-mismatch"})
					return nil, errSNIMismatch
				}
			}
			return p.Signer.leaf(host)
		},
		NextProtos: []string{"http/1.1"}, // NOTE: no h2; clients downgrade fine
	})
	defer tconn.Close()
	// the server's header timeout stopped applying at the hijack: bound the
	// handshake here, or a client that never finishes it holds the conn.
	_ = raw.SetDeadline(time.Now().Add(helloWait))
	if err := tconn.Handshake(); err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})

	br := bufio.NewReader(tconn)
	for {
		inner, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		fullURL := host + inner.URL.Path
		if inner.URL.RawQuery != "" {
			fullURL += "?" + inner.URL.RawQuery
		}
		ev := Event{Ev: "req", Host: host, Port: portStr(port), Kind: "mitm",
			Method: inner.Method, URL: fullURL, UA: inner.UserAgent()}
		if innerHost, _ := splitHostPort(inner.Host, port); !sameHost(innerHost, want) {
			ev.Rule, ev.Reason = "mitm", "host-mismatch"
			ev.Status = p.refuseTLS(tconn)
			p.audit(ev)
			return
		}
		v := p.Rule.Decide(host, port, inner.Method, inner.URL.Path, inner.URL.RawQuery)
		ev.Rule, ev.Reason = v.Rule, v.Reason
		if !v.Allowed {
			ev.Status = p.refuseTLS(tconn)
			p.audit(ev)
			return
		}

		start := time.Now()
		body := countBody(inner)
		inner.URL.Scheme = "https"
		inner.URL.Host = net.JoinHostPort(host, portStr(port))
		inner.RequestURI = ""
		resp, err := p.Upstream.RoundTrip(inner)
		if err != nil {
			ev.Reason, ev.Status = upstreamReason(err), http.StatusBadGateway
			ev.Out, ev.MS = body.n, time.Since(start).Milliseconds()
			p.audit(ev)
			(&http.Response{StatusCode: http.StatusBadGateway, ProtoMajor: 1, ProtoMinor: 1,
				Body: http.NoBody, Header: http.Header{}, Close: true}).Write(tconn)
			return
		}
		ev.Revocation = "unchecked"
		if resp.TLS != nil {
			ev.Revocation = stapleStatus(*resp.TLS)
		}
		// the upstream leg may be http/2, but the client side of this conn is
		// http/1.1 (see NextProtos above), so the response can't be written
		// verbatim: Write puts resp.Proto in the status line ("HTTP/2.0 200 OK"
		// on an http/1.1 stream), and when resp.ContentLength is -1 it adds
		// Transfer-Encoding: chunked while a stale upstream Content-Length
		// header line survives alongside -- double framing. lenient parsers
		// (curl, node) shrug both off; strict ones (rust hyper, e.g. github
		// copilot's auth stack) kill the request. renumber and let Write
		// re-derive the framing from resp.ContentLength alone.
		resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
		resp.Header.Del("Content-Length")
		resp.Header.Del("Transfer-Encoding")
		if resp.ContentLength < 0 {
			// unknown length must be chunked explicitly: Write would fall back
			// to close-delimiting, which stalls a keep-alive client forever.
			resp.TransferEncoding = []string{"chunked"}
		} else {
			resp.TransferEncoding = nil
		}
		in := &countReader{r: resp.Body}
		resp.Body = in
		err = resp.Write(tconn)
		resp.Body.Close()
		ev.Allowed, ev.Status, ev.In, ev.Out, ev.MS = true, resp.StatusCode, in.n, body.n, time.Since(start).Milliseconds()
		p.audit(ev)
		if err != nil || resp.Close || inner.Close {
			return
		}
	}
}

// sameHost compares a request host to the normalised CONNECT host.
func sameHost(got, want string) bool {
	n, err := NormalizeHost(got)
	return err == nil && n == want
}

// refuseTLS is refuse for a request inside a mitm'd connection: the same
// DenyResponse shapes, written through the tls conn. it returns the status
// sent, 0 for a reset.
func (p *Proxy) refuseTLS(tconn *tls.Conn) int {
	status := http.StatusForbidden
	switch p.DenyResponse {
	case "404":
		status = http.StatusNotFound
	case "timeout":
		time.Sleep(denyHold)
		fallthrough
	case "reset":
		if tc, ok := tconn.NetConn().(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		tconn.NetConn().Close()
		return 0
	}
	(&http.Response{StatusCode: status, ProtoMajor: 1, ProtoMinor: 1,
		Body: http.NoBody, Header: http.Header{}, Close: true}).Write(tconn)
	return status
}
