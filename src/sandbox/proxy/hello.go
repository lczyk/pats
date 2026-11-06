package proxy

import (
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"time"
)

// maxHello bounds how much of a client's opening bytes the tunnel buffers
// while looking for the ClientHello's server name.
const maxHello = 64 << 10

// helloWait bounds how long a tunnel waits for the client's first bytes.
const helloWait = 10 * time.Second

const (
	recordHandshake = 0x16
	hsClientHello   = 0x01
	extServerName   = 0x0000
	extECH          = 0xfe0d
)

// peekHello reads the client's opening bytes on a fresh tunnel and decides
// whether they may go to the CONNECT host. tls is required to name that host
// in its sni (a shared front end would otherwise serve any name under an
// allowed one), and a hello hidden behind ech cannot be checked and is refused.
// the bytes read are returned for the caller to forward unchanged; a non-tls
// opening on a port other than 443 returns nil and leaves them in the reader.
func (p *Proxy) peekHello(c bufConn, host string, port uint16) (hello []byte, reason string) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(helloWait))
	defer c.Conn.SetReadDeadline(time.Time{})

	// one byte classifies: a tls record starts with the handshake type, and
	// nothing raw (an "SSH-" banner, an http method) does. peeking more would
	// stall a raw client whose opening message is shorter than a record header.
	head, err := c.r.Peek(1)
	if err != nil {
		return nil, "no-hello"
	}
	if head[0] != recordHandshake {
		if port == 443 {
			return nil, "not-tls"
		}
		return nil, "" // raw, piped as-is
	}

	// the handshake message may span records: collect record payloads until
	// the message's own length is covered, keeping the raw records to replay.
	var raw, msg []byte
	need := -1
	for need < 0 || len(msg) < need {
		if len(raw) > maxHello {
			return nil, "hello-too-long"
		}
		rec := make([]byte, 5)
		if _, err := io.ReadFull(c.r, rec); err != nil {
			return nil, "no-hello"
		}
		if rec[0] != recordHandshake {
			return nil, "not-tls"
		}
		n := int(binary.BigEndian.Uint16(rec[3:5]))
		body := make([]byte, n)
		if _, err := io.ReadFull(c.r, body); err != nil {
			return nil, "no-hello"
		}
		raw = append(raw, rec...)
		raw = append(raw, body...)
		msg = append(msg, body...)
		if need < 0 && len(msg) >= 4 {
			if msg[0] != hsClientHello {
				return nil, "not-tls"
			}
			need = 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
		}
	}

	sni, ech, err := parseClientHello(msg[4:need])
	switch {
	case err != nil:
		return nil, "bad-hello"
	case ech:
		return nil, "ech"
	case sni == "":
		if _, err := netip.ParseAddr(host); err == nil {
			return raw, ""
		}
		return nil, "no-sni"
	}
	want, _ := NormalizeHost(host)
	if got, err := NormalizeHost(sni); err != nil || got != want {
		return nil, "sni-mismatch"
	}
	return raw, ""
}

var errHello = errors.New("malformed ClientHello")

// parseClientHello walks a ClientHello body (after the 4-byte handshake
// header) to its extensions and returns the sni, if any, and whether an
// encrypted_client_hello extension is present.
func parseClientHello(b []byte) (sni string, ech bool, err error) {
	skip := func(n int) bool {
		if len(b) < n {
			return false
		}
		b = b[n:]
		return true
	}
	vec := func(lenBytes int) ([]byte, bool) {
		if len(b) < lenBytes {
			return nil, false
		}
		var n int
		for i := 0; i < lenBytes; i++ {
			n = n<<8 | int(b[i])
		}
		b = b[lenBytes:]
		if len(b) < n {
			return nil, false
		}
		v := b[:n]
		b = b[n:]
		return v, true
	}
	if !skip(2 + 32) { // version, random
		return "", false, errHello
	}
	if _, ok := vec(1); !ok { // session id
		return "", false, errHello
	}
	if _, ok := vec(2); !ok { // cipher suites
		return "", false, errHello
	}
	if _, ok := vec(1); !ok { // compression methods
		return "", false, errHello
	}
	if len(b) == 0 {
		return "", false, nil // no extensions at all
	}
	exts, ok := vec(2)
	if !ok {
		return "", false, errHello
	}
	for len(exts) >= 4 {
		typ := binary.BigEndian.Uint16(exts[0:2])
		n := int(binary.BigEndian.Uint16(exts[2:4]))
		exts = exts[4:]
		if len(exts) < n {
			return "", false, errHello
		}
		data := exts[:n]
		exts = exts[n:]
		switch typ {
		case extECH:
			ech = true
		case extServerName:
			// list length(2), then entries of type(1) length(2) name.
			if len(data) < 2 {
				return "", false, errHello
			}
			list := data[2:]
			for len(list) >= 3 {
				t := list[0]
				l := int(binary.BigEndian.Uint16(list[1:3]))
				list = list[3:]
				if len(list) < l {
					return "", false, errHello
				}
				if t == 0 && sni == "" {
					sni = string(list[:l])
				}
				list = list[l:]
			}
		}
	}
	return sni, ech, nil
}
