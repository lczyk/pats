package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

// the key proxy (docs/spec/key-proxy.md): the sandbox holds a placeholder for
// each key its harness reads, and the proxy puts the real value in, in one
// header, on the endpoints the key is bound to. anywhere else it can see, a
// placeholder is refused.

// KeyBinding is one key the proxy holds for a pair: its env name (for the
// audit and `egress check`), the placeholder the sandbox was given, and the
// url entries it may be spent on. pats sets them; pats.yaml cannot.
type KeyBinding struct {
	Name        string   `yaml:"name" json:"name"`
	Placeholder string   `yaml:"placeholder" json:"placeholder"`
	Bind        []string `yaml:"bind" json:"bind"`
}

// heldKey is a binding with its real value and parsed entries.
type heldKey struct {
	name        string
	placeholder string
	real        string
	bind        []Entry
}

// maxKey bounds a real value, and minRandom the random part of a placeholder.
const (
	maxKey    = 512
	minRandom = 16
)

// CheckKey says whether a real value can be held: printable ascii, no space,
// at most maxKey bytes, and long enough past its prefix to stand in for.
func CheckKey(real string) error {
	if real == "" {
		return errors.New("empty")
	}
	if len(real) > maxKey {
		return fmt.Errorf("longer than %d bytes", maxKey)
	}
	for i := 0; i < len(real); i++ {
		if c := real[i]; c <= ' ' || c > '~' {
			return errors.New("not printable ascii without spaces")
		}
	}
	if n := len(real) - len(keyPrefix(real)); n < minRandom {
		return fmt.Errorf("%d characters past its prefix, under the %d a placeholder needs", n, minRandom)
	}
	return nil
}

// keyPrefix is the part of a key a placeholder keeps: up to and including its
// last "-", none without one.
func keyPrefix(real string) string {
	return real[:strings.LastIndexByte(real, '-')+1]
}

const (
	hexDigits = "0123456789abcdef"
	alnum     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// Placeholder is a stand-in for real: its prefix, then random characters of
// the alphabet of the rest (hex when the rest is lower-case hex, else letters
// and digits) to its length, so a cli that checks a key's shape takes it and
// a response scrubbed of the real value keeps its length. rnd nil is
// crypto/rand.
func Placeholder(real string, rnd io.Reader) (string, error) {
	if err := CheckKey(real); err != nil {
		return "", err
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	prefix := keyPrefix(real)
	rest := real[len(prefix):]
	alphabet := alnum
	if strings.Trim(rest, hexDigits) == "" {
		alphabet = hexDigits
	}
	b := []byte(prefix)
	for range len(rest) {
		i, err := rand.Int(rnd, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b = append(b, alphabet[i.Int64()])
	}
	if string(b) == real {
		return "", errors.New("placeholder came out equal to the key")
	}
	return string(b), nil
}

// ParseKeys reads PATS_EGRESS_KEYS: a json object from placeholder to real
// value.
func ParseKeys(s string) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, errors.New("not a json object of placeholder to key")
	}
	return m, nil
}

// holdKeys pairs each binding with its real value, failing on one without a
// value, a bad value or entry, or two bindings sharing a placeholder.
func holdKeys(bindings []KeyBinding, values map[string]string) ([]heldKey, error) {
	var out []heldKey
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.Name == "" || b.Placeholder == "" || len(b.Bind) == 0 {
			return nil, fmt.Errorf("key %q: a binding needs a name, a placeholder and an entry", b.Name)
		}
		if seen[b.Placeholder] {
			return nil, fmt.Errorf("key %s: placeholder used twice", b.Name)
		}
		seen[b.Placeholder] = true
		real, ok := values[b.Placeholder]
		if !ok {
			return nil, fmt.Errorf("key %s: no value for its placeholder", b.Name)
		}
		if err := CheckKey(real); err != nil {
			return nil, fmt.Errorf("key %s: %w", b.Name, err)
		}
		if len(real) != len(b.Placeholder) || real == b.Placeholder {
			return nil, fmt.Errorf("key %s: the placeholder does not stand in for its value", b.Name)
		}
		h := heldKey{name: b.Name, placeholder: b.Placeholder, real: real}
		for _, line := range b.Bind {
			e, err := ParseEntry(line, false)
			if err != nil {
				return nil, fmt.Errorf("key %s: bind %q: %w", b.Name, line, err)
			}
			if !e.URL() || e.methods == nil || e.ip.IsValid() || e.suffix {
				return nil, fmt.Errorf("key %s: bind %q: a binding names a method, a host and a path", b.Name, line)
			}
			h.bind = append(h.bind, e)
		}
		out = append(out, h)
	}
	return out, nil
}

// keyUse is what a request does with the held keys: nothing, spend one in
// its Authorization header, or misuse one.
type keyUse struct {
	key    *heldKey // the key spent, nil for none
	misuse bool
	bound  bool // the request is on a bound endpoint: its answer is scrubbed
}

// bearer is the one shape a placeholder is spent in.
const bearer = "Bearer "

// useOf decides what req does with the keys: a placeholder anywhere but an
// `Authorization: Bearer <placeholder>` on its binding, over tls, is a misuse,
// as is an Authorization of another shape on a bound endpoint. tls is
// whether the request came over a terminated connection.
func (p *Proxy) useOf(req *http.Request, host string, port uint16, tls bool) keyUse {
	if len(p.keys) == 0 {
		return keyUse{}
	}
	var u keyUse
	nhost, err := NormalizeHost(host)
	for i := range p.keys {
		k := &p.keys[i]
		if err == nil && tls && k.binds(nhost, port, req) {
			u.bound = true
			auth := req.Header.Values("Authorization")
			switch {
			case len(auth) == 0:
			case len(auth) == 1 && auth[0] == bearer+k.placeholder:
				u.key = k
			default:
				u.misuse = true
			}
		}
	}
	for i := range p.keys {
		k := &p.keys[i]
		if strings.Contains(req.URL.Path, k.placeholder) || strings.Contains(req.URL.RawPath, k.placeholder) ||
			strings.Contains(req.URL.RawQuery, k.placeholder) || strings.Contains(req.URL.Opaque, k.placeholder) ||
			strings.Contains(req.RequestURI, k.placeholder) {
			u.misuse = true
		}
		for name, vals := range req.Header {
			if strings.Contains(name, k.placeholder) {
				u.misuse = true
			}
			for _, v := range vals {
				if !strings.Contains(v, k.placeholder) {
					continue
				}
				if u.key != k || !strings.EqualFold(name, "Authorization") {
					u.misuse = true
				}
			}
		}
		for name, vals := range req.Trailer {
			if strings.Contains(name, k.placeholder) || slices.ContainsFunc(vals, func(v string) bool { return strings.Contains(v, k.placeholder) }) {
				u.misuse = true
			}
		}
	}
	if u.misuse {
		u.key = nil
	}
	return u
}

// binds says whether req, on host (normalised) and port, is on one of k's
// endpoints. the path must read one way only: a path an origin could read
// two ways is not one to spend a key on.
func (k *heldKey) binds(host string, port uint16, req *http.Request) bool {
	path := req.URL.Path
	if req.URL.Opaque != "" || !utf8.ValidString(path) || strings.ContainsFunc(path, isControl) || len(readings(path)) > 1 {
		return false
	}
	for _, e := range k.bind {
		if e.matchesHost(host, port) && e.matchesMethod(req.Method) && e.admitsURL(path, req.URL.RawQuery) {
			return true
		}
	}
	return false
}

func isControl(r rune) bool { return r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// keyHost says whether a key is bound on host and port: its tls is
// terminated, whatever the allow entries say.
func (p *Proxy) keyHost(host string, port uint16) bool {
	host, err := NormalizeHost(host)
	if err != nil {
		return false
	}
	for _, k := range p.keys {
		for _, e := range k.bind {
			if e.matchesHost(host, port) {
				return true
			}
		}
	}
	return false
}

// refuseKey refuses a request that misuses a held key: 403 whatever the
// policy's deny response, with a body that names neither the placeholder nor
// the query, audited.
func (p *Proxy) refuseKey(w http.ResponseWriter, ev *Event) {
	ev.Rule, ev.Reason, ev.Status = "key", "misuse", http.StatusForbidden
	http.Error(w, "egress denied: key misuse", http.StatusForbidden)
	p.audit(*ev)
}

// maskKeys is s with every placeholder written as "<placeholder>", for the
// audit.
func (p *Proxy) maskKeys(s string) string {
	for _, k := range p.keys {
		s = strings.ReplaceAll(s, k.placeholder, "<placeholder>")
	}
	return s
}

// errKeyScrub is an answer on a bound endpoint the proxy cannot read to
// scrub: encoded in a way go's transport did not undo.
var errKeyScrub = errors.New("an encoded answer on a bound endpoint")

// scrubHeader writes the placeholder over the real value in h.
func scrubHeader(h http.Header, k *heldKey) {
	for name, vals := range h {
		for i, v := range vals {
			if strings.Contains(v, k.real) {
				vals[i] = strings.ReplaceAll(v, k.real, k.placeholder)
			}
		}
		h[name] = vals
	}
}

// scrubBody is an answer's body with the real value written over by the
// placeholder as it streams: each read passes on at once all but the
// longest tail that could begin the real value. at the end that tail goes
// out as the placeholder's beginning of its length, and the trailers are
// scrubbed before the relay copies them; on an error it is dropped.
type scrubBody struct {
	r       io.ReadCloser
	key     *heldKey
	trailer func() http.Header
	buf     []byte
	out     []byte // ready to pass on
	tail    []byte // held: could begin the real value
	err     error
}

func newScrubBody(r io.ReadCloser, k *heldKey, trailer func() http.Header) *scrubBody {
	return &scrubBody{r: r, key: k, trailer: trailer, buf: make([]byte, 32<<10)}
}

func (s *scrubBody) Read(p []byte) (int, error) {
	for len(s.out) == 0 && s.err == nil {
		n, err := s.r.Read(s.buf)
		data := append(s.tail, s.buf[:n]...)
		data = bytes.ReplaceAll(data, []byte(s.key.real), []byte(s.key.placeholder))
		hold := heldTail(data, s.key.real)
		s.out, s.tail = data[:len(data)-hold], append([]byte(nil), data[len(data)-hold:]...)
		switch {
		case err == io.EOF:
			// what is held begins the real value: it goes out as the
			// placeholder's beginning.
			s.out = append(s.out, s.key.placeholder[:len(s.tail)]...)
			s.tail = nil
			if s.trailer != nil {
				if t := s.trailer(); t != nil {
					scrubHeader(t, s.key)
				}
			}
			s.err = io.EOF
		case err != nil:
			s.out, s.tail, s.err = nil, nil, err
		}
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	return 0, s.err
}

func (s *scrubBody) Close() error { return s.r.Close() }

// heldTail is the length of the longest proper beginning of real that data
// ends with.
func heldTail(data []byte, real string) int {
	for k := min(len(data), len(real)-1); k > 0; k-- {
		if bytes.HasSuffix(data, []byte(real[:k])) {
			return k
		}
	}
	return 0
}
