package proxy

import (
	"errors"
	"fmt"
	"net/netip"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Entry is one parsed allow or deny line:
//
//	[METHODS ]<host-entry>[/<glob> | ~<regexp>]
//
// the host entry is a literal host, a suffix (".x" or "*.x"), or an ip, each
// with an optional port. a glob or a regexp makes it a url entry, which also
// marks its host for tls termination. METHODS is a comma list of upper-case
// http methods, allowed only on a url entry (a tunnel cannot see a method).
type Entry struct {
	Raw     string
	host    string     // normalised; "" for an ip entry
	suffix  bool       // host matches itself and any subdomain
	ip      netip.Addr // valid for an ip entry
	port    uint16     // 0: 80 or 443
	methods []string   // nil: any
	urlRe   *regexp.Regexp
	deny    bool
}

// URL reports whether the entry filters on the path (and so mitm's its host).
func (e Entry) URL() bool { return e.urlRe != nil }

// Methods reports the entry's method prefix, nil for none.
func (e Entry) Methods() []string { return e.methods }

var methodsRe = regexp.MustCompile(`^[A-Z]+(,[A-Z]+)*$`)

// ParseEntry parses one allow or deny line. deny selects the case rule for the
// url part: a deny matches the path case-insensitively, an allow exactly.
func ParseEntry(raw string, deny bool) (Entry, error) {
	e := Entry{Raw: strings.TrimSpace(raw), deny: deny}
	s := e.Raw
	if s == "" {
		return e, errors.New("empty entry")
	}
	if first, rest, ok := strings.Cut(s, " "); ok && methodsRe.MatchString(first) {
		e.methods = strings.Split(first, ",")
		s = strings.TrimSpace(rest)
	}
	var hostPart, glob, re string
	if h, r, ok := strings.Cut(s, " ~"); ok {
		hostPart, re = strings.TrimSpace(h), r
		if re == "" {
			return e, errors.New("empty regexp after ~")
		}
	} else if h, g, ok := strings.Cut(s, "/"); ok {
		hostPart, glob = h, "/"+g
	} else {
		hostPart = s
	}
	if strings.ContainsAny(hostPart, " \t") {
		return e, fmt.Errorf("unexpected whitespace in %q", hostPart)
	}
	if err := e.parseHost(hostPart); err != nil {
		return e, err
	}
	switch {
	case re != "":
		if deny {
			re = "(?i)" + re
		}
		compiled, err := regexp.Compile(re)
		if err != nil {
			return e, fmt.Errorf("regexp: %w", err)
		}
		e.urlRe = compiled
	case glob != "":
		e.urlRe = globRe(glob, deny)
	}
	if e.methods != nil && e.urlRe == nil {
		return e, fmt.Errorf("a method needs a url entry: write %s %s/*", strings.Join(e.methods, ","), hostPart)
	}
	return e, nil
}

// parseHost fills host/suffix/ip/port from "[*.|.]host[:port]", "ip[:port]"
// or "[ipv6]:port".
func (e *Entry) parseHost(s string) error {
	if s == "" {
		return errors.New("missing host")
	}
	if ip, err := netip.ParseAddr(s); err == nil { // bare ip, v6 included
		e.ip = ip.Unmap()
		return nil
	}
	host, port := s, ""
	if strings.Contains(s, ":") {
		h, p, err := splitHostPortStrict(s)
		if err != nil {
			return err
		}
		host, port = h, p
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("bad port %q", port)
		}
		e.port = uint16(n)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		e.ip = ip.Unmap()
		return nil
	}
	if strings.HasPrefix(host, "*.") {
		host, e.suffix = host[1:], true
	}
	if strings.HasPrefix(host, ".") {
		host, e.suffix = host[1:], true
	}
	if host == "" || strings.Contains(host, "*") {
		return fmt.Errorf("host %q must be a literal hostname or a .suffix", s)
	}
	n, err := NormalizeHost(host)
	if err != nil {
		return fmt.Errorf("host %q: %w", host, err)
	}
	e.host = n
	return nil
}

// splitHostPortStrict is net.SplitHostPort for "host:port" and "[v6]:port",
// rejecting a bare v6 literal (the caller tried that form first).
func splitHostPortStrict(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 || end+1 >= len(s) || s[end+1] != ':' {
			return "", "", fmt.Errorf("bad host:port %q", s)
		}
		return s[1:end], s[end+2:], nil
	}
	if strings.Count(s, ":") != 1 {
		return "", "", fmt.Errorf("bad host:port %q", s)
	}
	host, port, _ = strings.Cut(s, ":")
	return host, port, nil
}

// globRe compiles a path glob ("*" matches anything, "/" included) to a full
// match over the path, with the query appended when the glob names one.
func globRe(glob string, deny bool) *regexp.Regexp {
	var b strings.Builder
	if deny {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	parts := strings.Split(glob, "*")
	for i, part := range parts {
		b.WriteString(regexp.QuoteMeta(part))
		if i < len(parts)-1 {
			b.WriteString(".*")
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// NormalizeHost canonicalises a hostname for matching: lowercase, one trailing
// dot stripped, idna to ascii. an ip literal comes back as its canonical text.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
	if h == "" {
		return "", errors.New("empty host")
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return ip.Unmap().String(), nil
	}
	a, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", err
	}
	return a, nil
}

// matchesHost reports whether the entry covers host (normalised) on port.
func (e Entry) matchesHost(host string, port uint16) bool {
	if e.port != 0 {
		if port != e.port {
			return false
		}
	} else if port != 80 && port != 443 {
		return false
	}
	if e.ip.IsValid() {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Unmap() == e.ip
	}
	if host == e.host {
		return true
	}
	return e.suffix && strings.HasSuffix(host, "."+e.host)
}

func (e Entry) matchesMethod(method string) bool {
	if e.methods == nil {
		return true
	}
	for _, m := range e.methods {
		if m == method {
			return true
		}
	}
	return false
}

// matchesURL tries the entry's url matcher against every candidate form of
// the request path: the path itself, the path with its query, and both again
// after path.Clean, so "a/../b" cannot dodge an entry anchored on "/b".
func (e Entry) matchesURL(urlPath, rawQuery string) bool {
	for _, c := range urlCandidates(urlPath, rawQuery) {
		if e.urlRe.MatchString(c) {
			return true
		}
	}
	return false
}

func urlCandidates(urlPath, rawQuery string) []string {
	cands := []string{urlPath}
	if rawQuery != "" {
		cands = append(cands, urlPath+"?"+rawQuery)
	}
	if cleaned := pathpkg.Clean(urlPath); urlPath != "" && cleaned != urlPath {
		cands = append(cands, cleaned)
		if rawQuery != "" {
			cands = append(cands, cleaned+"?"+rawQuery)
		}
	}
	return cands
}

// Rule is one egress policy: a default verdict and the parsed exceptions.
type Rule struct {
	DefaultAllow bool
	Allow        []Entry
	Deny         []Entry
	// Methods is the methods posture: nil passes every method, else a request
	// whose method is not in it is refused unless an allow entry's own method
	// prefix admits it. a posture makes every allowed host but the exempt ones
	// inspected, since a tunnel cannot show a method.
	Methods map[string]bool
	// Exempt are hosts the posture does not make inspected: the harness's own
	// (its inference api, token refresh). a request seen on one anyway (an
	// explicit url entry, plain http) is held to the posture.
	Exempt []Entry
}

// ParseRule parses allow and deny lines into a Rule, naming the first bad one.
func ParseRule(defaultAllow bool, allow, deny []string) (Rule, error) {
	r := Rule{DefaultAllow: defaultAllow}
	for _, line := range allow {
		e, err := ParseEntry(line, false)
		if err != nil {
			return r, fmt.Errorf("allow %q: %w", line, err)
		}
		r.Allow = append(r.Allow, e)
	}
	for _, line := range deny {
		e, err := ParseEntry(line, true)
		if err != nil {
			return r, fmt.Errorf("deny %q: %w", line, err)
		}
		r.Deny = append(r.Deny, e)
	}
	return r, nil
}

// Verdict is one policy decision with the entry that made it (or "default")
// and, on a refusal, why.
type Verdict struct {
	Allowed bool
	Rule    string
	Reason  string
}

// DecideHost is the verdict available before any request is seen: a bare deny
// entry on the host refuses, any allow entry on the host passes (a url entry's
// path check comes later), else the default. unparseable hosts are refused.
func (r Rule) DecideHost(host string, port uint16) Verdict {
	host, err := NormalizeHost(host)
	if err != nil {
		return Verdict{Rule: "default", Reason: "bad-host"}
	}
	for _, e := range r.Deny {
		if !e.URL() && e.matchesHost(host, port) {
			return Verdict{Rule: e.Raw}
		}
	}
	for _, e := range r.Allow {
		if e.matchesHost(host, port) {
			return Verdict{Allowed: true, Rule: e.Raw}
		}
	}
	return r.defaultVerdict()
}

// Decide is the full verdict for a request the proxy can see: deny entries
// first (bare or url, method included), then allow entries, with a host that
// has only url allow entries refused when none matches, then the default.
func (r Rule) Decide(host string, port uint16, method, urlPath, rawQuery string) Verdict {
	host, err := NormalizeHost(host)
	if err != nil {
		return Verdict{Rule: "default", Reason: "bad-host"}
	}
	for _, e := range r.Deny {
		if !e.matchesHost(host, port) || !e.matchesMethod(method) {
			continue
		}
		if !e.URL() || e.matchesURL(urlPath, rawQuery) {
			return Verdict{Rule: e.Raw}
		}
	}
	// an allow entry that names the request's method overrides the posture,
	// wherever it sits in the list; any other match is subject to it.
	var restricted, matched *Entry
	for i, e := range r.Allow {
		if !e.matchesHost(host, port) {
			continue
		}
		switch {
		case !e.URL():
			if matched == nil {
				matched = &r.Allow[i]
			}
		case e.matchesMethod(method) && e.matchesURL(urlPath, rawQuery):
			if e.methods != nil {
				return Verdict{Allowed: true, Rule: e.Raw}
			}
			if matched == nil {
				matched = &r.Allow[i]
			}
		case restricted == nil:
			restricted = &r.Allow[i]
		}
	}
	if matched != nil {
		return r.posture(method, Verdict{Allowed: true, Rule: matched.Raw})
	}
	if restricted != nil {
		return Verdict{Rule: restricted.Raw, Reason: "url-not-allowed"}
	}
	v := r.defaultVerdict()
	if v.Allowed {
		return r.posture(method, v)
	}
	return v
}

// posture applies the methods posture to an allow verdict: a method it does
// not list is refused, on any host it inspects.
func (r Rule) posture(method string, v Verdict) Verdict {
	if r.Methods == nil || r.Methods[method] {
		return v
	}
	return Verdict{Rule: "methods", Reason: "method"}
}

func (r Rule) exempt(host string, port uint16) bool {
	for _, e := range r.Exempt {
		if e.matchesHost(host, port) {
			return true
		}
	}
	return false
}

func (r Rule) defaultVerdict() Verdict {
	return Verdict{Allowed: r.DefaultAllow, Rule: "default"}
}

// MitmHost reports whether the host's tls must be terminated: some url entry
// names it, or a methods posture is set and the host is not exempt.
func (r Rule) MitmHost(host string, port uint16) bool {
	host, err := NormalizeHost(host)
	if err != nil {
		return false
	}
	for _, list := range [][]Entry{r.Deny, r.Allow} {
		for _, e := range list {
			if e.URL() && e.matchesHost(host, port) {
				return true
			}
		}
	}
	return r.Methods != nil && !r.exempt(host, port)
}
