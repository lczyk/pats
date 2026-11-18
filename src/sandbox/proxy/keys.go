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
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// the key proxy (docs/spec/key-proxy.md): the sandbox holds a placeholder for
// each key its harness reads, and the proxy puts the real value in, in one
// header, on the endpoints the key is bound to. anywhere else it can see, a
// placeholder is refused.

// KeyBinding is one key the proxy holds for a pair: its env name (for the
// audit and `egress check`), the placeholder the sandbox was given, and the
// endpoints it may be spent on. pats sets them; pats.yaml cannot.
type KeyBinding struct {
	Name        string     `yaml:"name" json:"name"`
	Placeholder string     `yaml:"placeholder" json:"placeholder"`
	Bind        []KeyEntry `yaml:"bind" json:"bind"`
}

// KeyEntry is one endpoint a key is bound to: a url entry, and the header
// the key goes out in there. with no Header that is `Authorization: Bearer
// <key>`; a named header carries the key as its whole value. yaml and json
// both carry an entry with no header as the line itself.
type KeyEntry struct {
	Match  string
	Header string
}

// KeyLines are endpoints that take the key as a bearer token, one for each
// line.
func KeyLines(lines ...string) []KeyEntry {
	if lines == nil {
		return nil
	}
	out := make([]KeyEntry, len(lines))
	for i, l := range lines {
		out[i] = KeyEntry{Match: l}
	}
	return out
}

// KeyMatches are the endpoints' url entries, without their headers.
func KeyMatches(entries []KeyEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Match
	}
	return out
}

// String is the entry with the header it spends the key in.
func (e KeyEntry) String() string {
	if e.Header == "" {
		return e.Match + " (Authorization: Bearer)"
	}
	return e.Match + " (" + textproto.CanonicalMIMEHeaderKey(e.Header) + ")"
}

// keyMapping is the mapping form on the wire.
type keyMapping struct {
	Match  string `yaml:"match" json:"match"`
	Header string `yaml:"header" json:"header"`
}

func (e KeyEntry) MarshalYAML() (any, error) {
	if e.Header == "" {
		return e.Match, nil
	}
	return keyMapping(e), nil
}

func (e KeyEntry) MarshalJSON() ([]byte, error) {
	if e.Header == "" {
		return json.Marshal(e.Match)
	}
	return json.Marshal(keyMapping(e))
}

var errKeyEntry = errors.New("a key's endpoint is a line, or a mapping of match and header")

// UnmarshalYAML reads a line or a mapping, its keys checked here: a
// decoder's own check for unknown fields stops at a type that unmarshals
// itself.
func (e *KeyEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*e = KeyEntry{Match: node.Value}
		return nil
	case yaml.MappingNode:
		var m KeyEntry
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if seen[k.Value] {
				return fmt.Errorf("line %d: %s is written twice in one endpoint", k.Line, k.Value)
			}
			seen[k.Value] = true
			if v.Kind != yaml.ScalarNode || v.Tag == "!!null" || v.Value == "" {
				return fmt.Errorf("line %d: %s: want a line", v.Line, k.Value)
			}
			switch k.Value {
			case "match":
				m.Match = v.Value
			case "header":
				m.Header = v.Value
			default:
				return fmt.Errorf("line %d: unknown field %q in a key's endpoint (it takes match and header)", k.Line, k.Value)
			}
		}
		if m.Match == "" || m.Header == "" {
			return fmt.Errorf("line %d: %w, both set", node.Line, errKeyEntry)
		}
		*e = m
		return nil
	}
	return fmt.Errorf("line %d: %w", node.Line, errKeyEntry)
}

// UnmarshalJSON reads what MarshalJSON writes, a key at a time: a struct's
// own decoding takes a key in any case and the last of two alike.
func (e *KeyEntry) UnmarshalJSON(data []byte) error {
	var line string
	if err := json.Unmarshal(data, &line); err == nil {
		*e = KeyEntry{Match: line}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errKeyEntry
	}
	var m KeyEntry
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, _ := tok.(string)
		if err != nil {
			return errKeyEntry
		}
		if seen[key] {
			return fmt.Errorf("%s is written twice in one endpoint", key)
		}
		seen[key] = true
		switch key {
		case "match":
			err = dec.Decode(&m.Match)
		case "header":
			err = dec.Decode(&m.Header)
		default:
			return fmt.Errorf("unknown field %q in a key's endpoint (it takes match and header)", key)
		}
		if err != nil {
			return fmt.Errorf("%w: %s", errKeyEntry, key)
		}
	}
	if m.Match == "" || m.Header == "" {
		return fmt.Errorf("%w, both set", errKeyEntry)
	}
	*e = m
	return nil
}

// heldKey is a binding with its real value and parsed endpoints.
type heldKey struct {
	name        string
	placeholder string
	real        string
	bind        []boundEntry
}

// boundEntry is an endpoint a key is spent on: the header that carries it,
// canonical, and what stands before it there.
type boundEntry struct {
	entry  Entry
	header string
	prefix string
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
		for _, ke := range b.Bind {
			be, err := ke.bound()
			if err != nil {
				return nil, fmt.Errorf("key %s: bind %q: %w", b.Name, ke.Match, err)
			}
			h.bind = append(h.bind, be)
		}
		out = append(out, h)
	}
	return out, nil
}

// Check says whether e can bind a key: a url entry with a method, on a host
// named in full (no suffix, no ip), and a header, when it names one, that is
// a header's name and not the one a bearer token goes in.
func (e KeyEntry) Check() error {
	_, err := e.bound()
	return err
}

func (e KeyEntry) bound() (boundEntry, error) {
	entry, err := ParseEntry(e.Match, false)
	if err != nil {
		return boundEntry{}, err
	}
	if !entry.URL() || entry.methods == nil || entry.ip.IsValid() || entry.suffix {
		return boundEntry{}, errors.New("a binding names a method, a host and a path")
	}
	if e.Header == "" {
		return boundEntry{entry: entry, header: "Authorization", prefix: bearer}, nil
	}
	if strings.ContainsFunc(e.Header, func(r rune) bool {
		return !(r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) {
		return boundEntry{}, fmt.Errorf("header %q is not a header's name", e.Header)
	}
	header := textproto.CanonicalMIMEHeaderKey(e.Header)
	if header == "Authorization" {
		return boundEntry{}, errors.New("header Authorization: an entry with no header already takes the key there, as a bearer token")
	}
	return boundEntry{entry: entry, header: header}, nil
}

// keyUse is what a request does with the held keys: nothing, spend one in
// its binding's header, or misuse one. scrub are the keys bound on its
// endpoint, whose real values its answer is scrubbed of.
type keyUse struct {
	key    *heldKey // the key spent, nil for none
	header string   // where it is spent, and what stands before it there
	prefix string
	misuse bool
	scrub  []*heldKey
}

// bearer stands before a placeholder spent in Authorization.
const bearer = "Bearer "

// useOf decides what req does with the keys. each endpoint a key is bound
// to names the one header that spends it there: `Authorization: Bearer
// <placeholder>`, or the placeholder as another header's whole value. a
// placeholder anywhere else is a misuse, as is that header in another shape
// on a bound endpoint. a binding is on tls (tls: the request came over a
// terminated connection) to port 443, with the request's own Host naming
// that port.
func (p *Proxy) useOf(req *http.Request, host string, port uint16, tls bool) keyUse {
	if len(p.keys) == 0 {
		return keyUse{}
	}
	var u keyUse
	nhost, err := NormalizeHost(host)
	_, hostPort := splitHostPort(req.Host, port)
	var here []*boundEntry // the endpoint of each of u.scrub's keys, in step
	if err == nil && tls && port == 443 && hostPort == port {
		for i := range p.keys {
			k := &p.keys[i]
			if b := k.bound(nhost, port, req); b != nil {
				u.scrub, here = append(u.scrub, k), append(here, b)
			}
		}
	}
	// a header that spends a key here carries one value, the placeholder of
	// a key bound here to that header, and one key at most is spent.
	asked := map[string]bool{}
	for _, b := range here {
		if asked[b.header] {
			continue
		}
		asked[b.header] = true
		vals := req.Header.Values(b.header)
		if len(vals) == 0 {
			continue
		}
		spent := -1
		for i, o := range here {
			if len(vals) == 1 && o.header == b.header && vals[0] == o.prefix+u.scrub[i].placeholder {
				spent = i
			}
		}
		if spent < 0 || u.key != nil {
			u.misuse = true
			continue
		}
		u.key, u.header, u.prefix = u.scrub[spent], b.header, here[spent].prefix
	}
	// what the url says, decoded once more: a placeholder escaped in it is
	// still in it.
	where := []string{req.URL.Path, req.URL.RawPath, req.URL.RawQuery, req.URL.Opaque, req.RequestURI}
	if q, err := url.QueryUnescape(req.URL.RawQuery); err == nil {
		where = append(where, q)
	}
	if ep, err := url.PathUnescape(req.URL.Path); err == nil {
		where = append(where, ep)
	}
	for i := range p.keys {
		k := &p.keys[i]
		if slices.ContainsFunc(where, func(s string) bool { return strings.Contains(s, k.placeholder) }) {
			u.misuse = true
		}
		lower := strings.ToLower(k.placeholder)
		for name, vals := range req.Header {
			// a header name comes canonicalised: compared without case.
			if strings.Contains(strings.ToLower(name), lower) {
				u.misuse = true
			}
			for _, v := range vals {
				if strings.Contains(v, k.placeholder) && (u.key != k || !strings.EqualFold(name, u.header)) {
					u.misuse = true
				}
			}
		}
		for name := range req.Trailer {
			if strings.Contains(strings.ToLower(name), lower) {
				u.misuse = true
			}
		}
	}
	if u.misuse {
		u.key = nil
	}
	return u
}

// bound is the first of k's endpoints req is on, on host (normalised) and
// port, nil for none. the path must be spelled plainly and read one way
// only: a path an origin could read two ways is not one to spend a key on.
func (k *heldKey) bound(host string, port uint16, req *http.Request) *boundEntry {
	path := req.URL.Path
	target, _, _ := strings.Cut(req.RequestURI, "?")
	if req.URL.Opaque != "" || req.URL.RawPath != "" || strings.ContainsRune(target, '%') ||
		!utf8.ValidString(path) || strings.ContainsFunc(path, isControl) || len(readings(path)) > 1 {
		return nil
	}
	for i := range k.bind {
		e := &k.bind[i].entry
		if e.matchesHost(host, port) && e.matchesMethod(req.Method) && e.admitsURL(path, req.URL.RawQuery) {
			return &k.bind[i]
		}
	}
	return nil
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
		for _, b := range k.bind {
			if b.entry.matchesHost(host, port) {
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

// scrubbed is s with every real value in it written over by its
// placeholder, again until none is left: a placeholder next to what follows
// it can spell a real value anew. a value that keeps coming back is masked.
func scrubbed(s string, k *heldKey) string {
	for range 8 {
		if !strings.Contains(s, k.real) {
			return s
		}
		s = strings.ReplaceAll(s, k.real, k.placeholder)
	}
	return strings.ReplaceAll(s, k.real, strings.Repeat("*", len(k.real)))
}

// scrubbedName is a header name with the real value written over, its case
// ignored: a name comes canonicalised ("X-Sk-Or-V1-...").
func scrubbedName(name string, k *heldKey) string {
	lower, real := strings.ToLower(name), strings.ToLower(k.real)
	for range 8 {
		i := strings.Index(lower, real)
		if i < 0 {
			return name
		}
		name = name[:i] + k.placeholder + name[i+len(real):]
		lower = strings.ToLower(name)
	}
	return strings.Repeat("*", len(name))
}

// scrubHeader writes the placeholder over the real value in h, in names
// (an announced trailer's among them) and values.
func scrubHeader(h http.Header, k *heldKey) {
	for name, vals := range h {
		for i, v := range vals {
			vals[i] = scrubbed(v, k)
		}
		if n := scrubbedName(name, k); n != name {
			delete(h, name)
			h[n] = vals
		}
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
		data = []byte(scrubbed(string(data), s.key))
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
