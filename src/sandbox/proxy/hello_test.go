package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// streamConn is the client side of a tunnel as peekHello sees it: bytes to
// read and a deadline to set, with no socket behind it.
type streamConn struct{ net.Conn }

func (streamConn) SetReadDeadline(time.Time) error { return nil }

func peekOn(stream []byte, host string, port uint16) (hello []byte, reason string, rest *bufio.Reader) {
	rest = bufio.NewReader(bytes.NewReader(stream))
	hello, reason = (&Proxy{}).peekHello(bufConn{Conn: streamConn{}, r: rest}, host, port)
	return hello, reason, rest
}

// helloBody is the ClientHello a go tls client sends for serverName, without
// its record and handshake headers.
func helloBody(t testing.TB, serverName string) []byte {
	raw := clientHello(t, serverName)
	need := 4 + (int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8]))
	return raw[9 : 5+need]
}

// asRecord wraps a ClientHello body in its handshake header and one record.
func asRecord(body []byte) []byte {
	n := len(body)
	msg := append([]byte{hsClientHello, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
	return append([]byte{recordHandshake, 3, 1, byte(len(msg) >> 8), byte(len(msg))}, msg...)
}

var errHelloSeen = errors.New("hello seen")

// serverName is the name crypto/tls's server reads from the records a client
// opens with, and whether it took the hello at all: the other parser of the
// same bytes, standing in for the front end a tunnel reaches.
func serverName(records []byte) (name string, ok bool) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	// the server answers a hello it refuses with an alert: without a reader
	// on the client side neither end of the pipe would move.
	go func() { _, _ = c1.Write(records) }()
	go func() { _, _ = io.Copy(io.Discard, c1) }()
	srv := tls.Server(c2, &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		name, ok = chi.ServerName, true
		return nil, errHelloSeen
	}})
	_ = c2.SetDeadline(time.Now().Add(time.Second))
	_ = srv.Handshake()
	return name, ok
}

// FuzzParseClientHello: the hello is the client's own bytes, and the tunnel
// is held to the name read from it. so the name the proxy reads must be the
// name a server reads: a hello crypto/tls takes parses here too, to the same
// server name (or is refused for its ech).
func FuzzParseClientHello(f *testing.F) {
	for _, name := range []string{"good.test", "", "xn--bcher-kva.example", "a.b.c.d.e.test"} {
		f.Add(helloBody(f, name))
	}
	f.Add([]byte{1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		sni, ech, err := parseClientHello(body)
		if len(body) > 16000 {
			return // more than one record: the fragmenting is FuzzPeekHello's
		}
		name, ok := serverName(asRecord(body))
		if !ok {
			return
		}
		assert.NoError(t, err, "crypto/tls takes a hello the proxy calls malformed")
		if !ech {
			assert.Equal(t, sni, name, "the proxy and the server read different names")
		}
	})
}

var peekReasons = map[string]bool{
	"": true, "no-hello": true, "not-tls": true, "hello-too-long": true,
	"bad-hello": true, "ech": true, "no-sni": true, "sni-mismatch": true,
}

// FuzzPeekHello: whatever a client opens a tunnel with, the bytes the proxy
// forwards are the client's own, unchanged and bounded, a refusal forwards
// none, and a server reading them finds the CONNECT host's name (or none,
// for an ip).
func FuzzPeekHello(f *testing.F) {
	good := clientHello(f, "good.test")
	f.Add(good, "good.test", uint16(443))
	f.Add(good, "GOOD.test.", uint16(8443))
	f.Add(good, "other.test", uint16(443))
	f.Add(fragment(good, 7), "good.test", uint16(443))
	f.Add(clientHello(f, ""), "192.0.2.1", uint16(443))
	f.Add(clientHello(f, ""), "good.test", uint16(443))
	f.Add([]byte("SSH-2.0-x\r\n"), "good.test", uint16(22))
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"), "good.test", uint16(443))
	f.Add([]byte{recordHandshake, 3, 1, 0, 0}, "good.test", uint16(443))
	f.Add([]byte{recordHandshake, 3, 1, 0xff, 0xff, hsClientHello, 0xff, 0xff, 0xff}, "good.test", uint16(443))
	f.Add([]byte{}, "", uint16(0))
	f.Fuzz(func(t *testing.T, stream []byte, host string, port uint16) {
		hello, reason, rest := peekOn(stream, host, port)
		assert.That(t, peekReasons[reason], "unknown reason %q", reason)
		if reason != "" {
			assert.That(t, hello == nil, "a refused opening forwards bytes")
			return
		}
		if hello == nil {
			// raw: not tls, off 443, and every byte still there to pipe.
			assert.That(t, port != 443, "a raw opening passed on 443")
			left, _ := io.ReadAll(rest)
			assert.That(t, bytes.Equal(left, stream), "a raw opening lost bytes")
			return
		}
		assert.That(t, bytes.HasPrefix(stream, hello), "the hello forwarded is not the client's")
		assert.That(t, len(hello) <= maxHello+5+0xffff, "hello of %d bytes held", len(hello))
		name, ok := serverName(hello)
		if !ok {
			return
		}
		if name == "" {
			_, err := netip.ParseAddr(host)
			assert.NoError(t, err, "a hello naming no host passed for %q", host)
			return
		}
		want, err := NormalizeHost(host)
		assert.NoError(t, err)
		got, err := NormalizeHost(name)
		assert.NoError(t, err)
		assert.Equal(t, got, want, "the server reads a name the tunnel was not opened for")
	})
}

// BenchmarkPeekHello is the work a tunnel does before its first byte goes
// upstream: the records read and the hello walked to its server name.
func BenchmarkPeekHello(b *testing.B) {
	hello := clientHello(b, "good.test")
	p := &Proxy{}
	stream := bytes.NewReader(hello)
	c := bufConn{Conn: streamConn{}, r: bufio.NewReader(stream)}
	b.ReportAllocs()
	b.SetBytes(int64(len(hello)))
	for b.Loop() {
		stream.Reset(hello)
		c.r.Reset(stream)
		if got, reason := p.peekHello(c, "good.test", 443); got == nil {
			b.Fatal(reason)
		}
	}
}
