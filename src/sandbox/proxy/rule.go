package proxy

import (
	"errors"
	"fmt"
	"net/netip"
	pathpkg "path"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
	queryRe *regexp.Regexp // an allow's urlRe over path?query: see spelledQuery
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
		compiled, err := globRe(glob, deny)
		if err != nil {
			return e, fmt.Errorf("glob: %w", err)
		}
		e.urlRe = compiled
	}
	if e.urlRe != nil && !deny {
		var err error
		if e.queryRe, err = spelledQuery(e.urlRe); err != nil {
			return e, fmt.Errorf("regexp: %w", err)
		}
	}
	if e.methods != nil && e.urlRe == nil {
		return e, fmt.Errorf("a method needs a url entry: write %s %s/**", strings.Join(e.methods, ","), hostPart)
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
		e.ip = ip.Unmap().WithZone("")
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
		e.ip = ip.Unmap().WithZone("")
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
	if ip, err := netip.ParseAddr(n); err == nil {
		// an address written with the root's dot.
		if e.suffix {
			return fmt.Errorf("host %q: an address has no subdomains", s)
		}
		e.ip = ip
		return nil
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

// globRe compiles a path glob to a full match over the path, with the query
// appended when the glob names one. "*" matches within one path segment, "/"
// excluded, and "**" matches anything, "/" included, as gitignore reads them:
// "github.com/*/repo**" pins the owner segment and takes all under the repo.
func globRe(glob string, deny bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if deny {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch {
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			for i < len(glob) && glob[i] == '*' {
				i++
			}
		case glob[i] == '*':
			b.WriteString("[^/]*")
			i++
		default:
			j := strings.IndexByte(glob[i:], '*')
			if j < 0 {
				j = len(glob) - i
			}
			b.WriteString(regexp.QuoteMeta(glob[i : i+j]))
			i += j
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// NormalizeHost canonicalises a hostname for matching: idna to ascii (which
// maps case as a resolver does), the root's trailing dot stripped. an ip
// literal comes back as its canonical text. there is one form per name, so
// what comes out normalises to itself, and what is no name is refused, never
// turned into one: an empty label ("a..b", ".a", "a.."), a label or a name
// too long for dns, and a name ending in a number that is no address
// ("127.1", "0x7f000001"), which some resolvers read as one.
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if ip, err := netip.ParseAddr(strings.Trim(strings.TrimSuffix(h, "."), "[]")); err == nil {
		// without its zone, which names an interface of the client's.
		return ip.Unmap().WithZone("").String(), nil
	}
	// idna first: it is what turns a full-width dot into a dot, and what
	// lowers a letter the way the name is looked up (strings.ToLower lowers
	// a dotted capital I to another name).
	a, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", err
	}
	a = strings.ToLower(strings.TrimSuffix(a, "."))
	if a == "" {
		return "", errors.New("empty host")
	}
	if len(a) > 253 {
		return "", fmt.Errorf("host %q is longer than a name can be", h)
	}
	labels := strings.Split(a, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("host %q has an empty or an overlong label", h)
		}
	}
	if last := labels[len(labels)-1]; numeric(last) {
		return "", fmt.Errorf("host %q ends in a number and is no address", h)
	}
	return a, nil
}

// numeric says whether a label reads as a number to inet_aton: digits, or
// hex behind 0x.
func numeric(label string) bool {
	digits := "0123456789"
	if rest, ok := strings.CutPrefix(label, "0x"); ok {
		label, digits = rest, "0123456789abcdef"
	}
	return strings.Trim(label, digits) == ""
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
		// a zone names an interface of the client's, not a destination.
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Unmap().WithZone("") == e.ip
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

// matchesURL is a deny's reading of a request: the entry's url matcher is
// tried against every candidate form of the path, each reading of it alone
// and with its query, so "a/../b" cannot dodge an entry anchored on "/b".
func (e Entry) matchesURL(urlPath, rawQuery string) bool {
	for _, c := range urlCandidates(urlPath, rawQuery) {
		if e.urlRe.MatchString(c) {
			return true
		}
	}
	return false
}

func urlCandidates(urlPath, rawQuery string) []string {
	var cands []string
	for _, p := range append([]string{urlPath, pathpkg.Clean(urlPath)}, readings(urlPath)...) {
		if (p == "." && urlPath == "") || slices.Contains(cands, p) {
			continue // path.Clean("") is ".", which no request asked for
		}
		cands = append(cands, p)
		if rawQuery != "" {
			cands = append(cands, p+"?"+rawQuery)
		}
	}
	return cands
}

// readings are the paths an origin may serve for urlPath: the path as sent,
// the path resolved (resolvePath), and both again as an origin reads a
// segment's ";parameters" and a backslash when it has either: a servlet
// container drops the parameters before it resolves the path, and some
// servers take a backslash for a slash, so "/public/..;/private" and
// "/public/..\private" are "/private" to those.
func readings(urlPath string) []string {
	out := []string{urlPath}
	if urlPath == "" {
		return out
	}
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	add(resolvePath(urlPath))
	slashed := strings.ReplaceAll(urlPath, `\`, "/")
	for _, as := range []string{dropParams(urlPath), slashed, dropParams(slashed)} {
		add(as)
		add(resolvePath(as))
	}
	return out
}

// dropParams is urlPath with each segment cut at its first ";".
func dropParams(urlPath string) string {
	if !strings.Contains(urlPath, ";") {
		return urlPath
	}
	segs := strings.Split(urlPath, "/")
	for i, seg := range segs {
		segs[i], _, _ = strings.Cut(seg, ";")
	}
	return strings.Join(segs, "/")
}

// resolvePath is the path an origin serves for urlPath: dot segments and
// doubled slashes gone, as path.Clean leaves it, with the trailing slash a
// directory keeps ("/a/b/.." is "/a/").
func resolvePath(urlPath string) string {
	if urlPath == "" {
		return ""
	}
	r := pathpkg.Clean(urlPath)
	for _, dir := range []string{"/", "/.", "/.."} {
		if strings.HasSuffix(urlPath, dir) && r != "/" {
			return r + "/"
		}
	}
	return r
}

// admitsURL is an allow's reading of a request, which errs the other way to
// matchesURL: the entry admits a path only when every reading of it matches,
// so "/public/../private" does not pass for "/public/**".
func (e Entry) admitsURL(urlPath, rawQuery string) bool {
	for _, p := range readings(urlPath) {
		if !e.admitsPath(p, rawQuery) {
			return false
		}
	}
	return true
}

// querySep stands for the "?" between a path and its query in what queryRe
// is matched against: a rune of its own, so that "?" can be told from one
// inside the query. a request that holds the rune itself matches nothing.
const querySep = '\uE000'

// admitsPath matches one reading of a path: alone, or with the query behind
// it when the entry spells the "?" between them. the match must begin in the
// path or at that "?": one inside the query would let a query stand in for
// the path the entry asks for.
func (e Entry) admitsPath(p, rawQuery string) bool {
	if e.urlRe.MatchString(p) {
		return true
	}
	if rawQuery == "" || strings.ContainsRune(p, querySep) || strings.ContainsRune(rawQuery, querySep) {
		return false
	}
	loc := e.queryRe.FindStringIndex(p + string(querySep) + rawQuery)
	return loc != nil && loc[0] <= len(p)
}

// spelledQuery is re for matching a path and its query joined by querySep:
// a "?" the entry spells matches the join as well as a "?", and no wildcard
// does. so the query is reached only through a spelled "?", and a "*" or a
// "[^/]+" written for a path segment cannot take the join and what follows
// it ("/repos/?/x" for "/repos/*/x"), while past it a "?" is a character
// like any other.
func spelledQuery(re *regexp.Regexp) (*regexp.Regexp, error) {
	tree, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil, err
	}
	return regexp.Compile(spellQuery(tree).String())
}

func spellQuery(re *syntax.Regexp) *syntax.Regexp {
	spelled := func() *syntax.Regexp {
		return &syntax.Regexp{Op: syntax.OpCharClass, Rune: []rune{'?', '?', querySep, querySep}}
	}
	switch re.Op {
	case syntax.OpAnyChar:
		re.Op, re.Rune = syntax.OpCharClass, []rune{0, querySep - 1, querySep + 1, unicode.MaxRune}
	case syntax.OpAnyCharNotNL:
		re.Op, re.Rune = syntax.OpCharClass, []rune{0, '\n' - 1, '\n' + 1, querySep - 1, querySep + 1, unicode.MaxRune}
	case syntax.OpCharClass:
		// a class that lists "?" by itself spells it; a range is a wildcard.
		var out []rune
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			switch {
			case lo == '?' && hi == '?':
				out = append(out, lo, hi, querySep, querySep)
			case querySep < lo || hi < querySep:
				out = append(out, lo, hi)
			default:
				if lo < querySep {
					out = append(out, lo, querySep-1)
				}
				if querySep < hi {
					out = append(out, querySep+1, hi)
				}
			}
		}
		// the join is listed after the "?" it stands for: back in order.
		pairs := make([][2]rune, 0, len(out)/2)
		for i := 0; i+1 < len(out); i += 2 {
			pairs = append(pairs, [2]rune{out[i], out[i+1]})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
		out = out[:0]
		for _, pr := range pairs {
			out = append(out, pr[0], pr[1])
		}
		if re.Rune = out; len(out) == 0 {
			re.Op = syntax.OpNoMatch
		}
	case syntax.OpLiteral:
		if !slices.Contains(re.Rune, '?') && !slices.Contains(re.Rune, querySep) {
			break
		}
		var parts []*syntax.Regexp
		start := 0
		for i, r := range re.Rune {
			if r != '?' && r != querySep {
				continue
			}
			if i > start {
				parts = append(parts, &syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: slices.Clone(re.Rune[start:i])})
			}
			if r == '?' {
				parts = append(parts, spelled())
			} else {
				// the rune itself, written into an entry, is not the join.
				parts = append(parts, &syntax.Regexp{Op: syntax.OpNoMatch})
			}
			start = i + 1
		}
		if start < len(re.Rune) {
			parts = append(parts, &syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: slices.Clone(re.Rune[start:])})
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return &syntax.Regexp{Op: syntax.OpConcat, Sub: parts}
	}
	for i, sub := range re.Sub {
		re.Sub[i] = spellQuery(sub)
	}
	return re
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
		case e.matchesMethod(method) && e.admitsURL(urlPath, rawQuery):
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
