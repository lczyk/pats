// mitm: for hosts named by a url entry the proxy terminates tls with a leaf
// signed by the per-run CA, filters each request by full url, and re-dials the
// real host. everything else stays a tunnel (see proxy.go).
package proxy

import (
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
	"net/url"
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
// serves the decrypted connection as http: each request is filtered by url
// and the allowed ones forwarded to the real host via Upstream. the leaf is
// only ever for the CONNECT host and each inner request's Host must name it
// too: a shared front end must not be reached under a name the policy never
// saw.
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
	// the 200 must go straight to the wire, not through WriteHeader -- see the
	// matching comment in handleConnect (proxy.go).
	if _, err := raw.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		raw.Close()
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
	// the server's header timeout stopped applying at the hijack: bound the
	// handshake here, or a client that never finishes it holds the conn.
	_ = raw.SetDeadline(time.Now().Add(helloWait))
	if err := tconn.Handshake(); err != nil {
		// an sni mismatch is audited where it is caught. anything else that
		// fails here (a non-tls client, a client rejecting the leaf) would
		// otherwise leave no trace.
		if !errors.Is(err, errSNIMismatch) {
			reason := "handshake"
			var rh tls.RecordHeaderError
			if errors.As(err, &rh) {
				reason = "not-tls"
			}
			p.audit(Event{Ev: "open", Host: host, Port: portStr(port), Kind: "mitm", Rule: "mitm", Reason: reason})
		}
		tconn.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	origin := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, portStr(port))}
	idle := p.Idle
	if idle <= 0 {
		idle = idleWait
	}
	serveConn(tconn, idle, http.HandlerFunc(func(w http.ResponseWriter, inner *http.Request) {
		ev := Event{Ev: "req", Host: host, Port: portStr(port), Kind: "mitm",
			Method: inner.Method, URL: auditURL(host, inner.URL), UA: inner.UserAgent()}
		if innerHost, _ := splitHostPort(inner.Host, port); !sameHost(innerHost, want) {
			ev.Rule, ev.Reason = "mitm", "host-mismatch"
			ev.Status = p.refuse(w)
			p.audit(ev)
			return
		}
		v := p.Rule.Decide(host, port, inner.Method, inner.URL.Path, inner.URL.RawQuery)
		ev.Rule, ev.Reason = v.Rule, v.Reason
		if !v.Allowed {
			ev.Status = p.refuse(w)
			p.audit(ev)
			return
		}
		p.forward(w, inner, ev, origin)
	}))
}

// idleWait is how long a client's connection may sit between requests.
const idleWait = 2 * time.Minute

// serveConn serves the one connection c as net/http serves any it accepts:
// its framing, its keep-alive and what a handler left unread are the
// server's. it returns once the connection is closed, or a handler took it
// over (a refusal by reset). idle bounds its silences.
func serveConn(c net.Conn, idle time.Duration, h http.Handler) {
	ln := &oneConn{conn: c, done: make(chan struct{})}
	h1 := new(http.Protocols)
	h1.SetHTTP1(true)
	srv := &http.Server{
		Handler:   h,
		Protocols: h1,
		// "OPTIONS *" is a request like any other: decided, relayed, audited.
		DisableGeneralOptionsHandler: true,
		// the wait for a connection's first request counts as its headers'.
		ReadHeaderTimeout: idle,
		IdleTimeout:       idle,
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				ln.Close()
			}
		},
	}
	_ = srv.Serve(ln)
}

// oneConn is a listener of one connection: accepted once, and then nothing
// more until it is closed.
type oneConn struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	if c := l.conn; c != nil {
		l.conn = nil
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return oneAddr{} }

type oneAddr struct{}

func (oneAddr) Network() string { return "conn" }
func (oneAddr) String() string  { return "conn" }

// sameHost compares a request host to the normalised CONNECT host.
func sameHost(got, want string) bool {
	n, err := NormalizeHost(got)
	return err == nil && n == want
}
