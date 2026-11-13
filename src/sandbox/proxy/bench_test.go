package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// countListener counts the writes the proxy makes to its clients. each one
// is a syscall, and on a mitm'd connection a tls record of its own.
type countListener struct {
	net.Listener
	writes *atomic.Int64
}

func (l countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countConn{Conn: c, writes: l.writes}, nil
}

type countConn struct {
	net.Conn
	writes *atomic.Int64
}

func (c *countConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

// chunks is a response body of unknown length that arrives in n pieces, as
// a model's event stream does.
type chunks struct {
	piece []byte
	n     int
}

func (c *chunks) Read(p []byte) (int, error) {
	if c.n == 0 {
		return 0, io.EOF
	}
	c.n--
	return copy(p, c.piece), nil
}

// BenchmarkMitm is one request on a connection already open and terminated
// here: read, decided, answered by a stub upstream and written back, which
// is everything the proxy adds to a request it inspects. writes/op is the
// records the answer went out in.
func BenchmarkMitm(b *testing.B) {
	header := http.Header{"Content-Type": {"application/json"}}
	for i := range 24 {
		header.Set(fmt.Sprintf("X-Upstream-Header-%d", i), "a value of a usual length")
	}
	shapes := []struct {
		name string
		body func() (io.Reader, int64)
	}{
		{"json-2k", func() (io.Reader, int64) { return bytes.NewReader(make([]byte, 2<<10)), 2 << 10 }},
		{"stream-200x200", func() (io.Reader, int64) { return &chunks{piece: make([]byte, 200), n: 200}, -1 }},
	}
	for _, shape := range shapes {
		b.Run(shape.name, func(b *testing.B) {
			p, pool, _ := mitmProxy(b, "api.test", rtFunc(func(*http.Request) (*http.Response, error) {
				body, n := shape.body()
				return &http.Response{StatusCode: 200, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
					Header: header.Clone(), Body: io.NopCloser(body), ContentLength: n}, nil
			}))
			p.Audit = io.Discard
			var writes atomic.Int64
			srv := httptest.NewUnstartedServer(p)
			srv.Listener = countListener{srv.Listener, &writes}
			srv.Start()
			defer srv.Close()
			proxyURL, _ := url.Parse(srv.URL)
			client := &http.Client{Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
			defer client.CloseIdleConnections()
			get := func() {
				resp, err := client.Get("https://api.test/v1/messages?beta=true")
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			get() // the tunnel, the handshake and the leaf are paid once
			writes.Store(0)
			b.ReportAllocs()
			for b.Loop() {
				get()
			}
			b.ReportMetric(float64(writes.Load())/float64(b.N), "writes/op")
		})
	}
}

// BenchmarkTunnel is the bytes a blind tunnel moves, there and back through
// an echoing upstream: the two copy loops every byte of a tunnelled
// connection crosses.
func BenchmarkTunnel(b *testing.B) {
	ln := newEchoListener(b)
	defer ln.Close()
	echoAddr := ln.Addr().String()
	srv := httptest.NewServer(&Proxy{Rule: mustRule(b, false, []string{echoAddr}, nil), Dial: looseDial})
	defer srv.Close()
	conn, br := connect(b, srv.Listener.Addr().String(), echoAddr)
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})

	chunk := bytes.Repeat([]byte("x"), 32<<10)
	back := make([]byte, len(chunk))
	b.SetBytes(int64(2 * len(chunk)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := conn.Write(chunk); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(br, back); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLeaf is the certificate a mitm'd handshake waits for: minted for
// a host not seen yet, cached after. the parallel runs show what handshakes
// to different hosts cost each other.
func BenchmarkLeaf(b *testing.B) {
	certPath, keyPath, _ := testCA(b, b.TempDir())
	signer := func(b *testing.B) *Signer {
		s, err := NewSigner(certPath, keyPath)
		if err != nil {
			b.Fatal(err)
		}
		return s
	}
	var host atomic.Int64
	fresh := func() string { return fmt.Sprintf("h%d.example.com", host.Add(1)) }
	b.Run("mint", func(b *testing.B) {
		s := signer(b)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.leaf(fresh()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("mint-parallel", func(b *testing.B) {
		s := signer(b)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := s.leaf(fresh()); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
	b.Run("cached", func(b *testing.B) {
		s := signer(b)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.leaf("api.example.com"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached-parallel", func(b *testing.B) {
		s := signer(b)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := s.leaf("api.example.com"); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}
