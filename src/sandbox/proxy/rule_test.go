package proxy

import (
	"fmt"
	"net/netip"
	pathpkg "path"
	"regexp"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func mustRule(t testing.TB, defaultAllow bool, allow, deny []string) Rule {
	t.Helper()
	r, err := ParseRule(defaultAllow, allow, deny)
	require.NoError(t, err)
	return r
}

func TestParseEntry(t *testing.T) {
	ok := []struct {
		raw         string
		host        string
		suffix, url bool
		port        uint16
		methods     []string
		ip          string
	}{
		{raw: "github.com", host: "github.com"},
		{raw: ".ubuntu.com", host: "ubuntu.com", suffix: true},
		{raw: "*.npmjs.org", host: "npmjs.org", suffix: true},
		{raw: "github.com:22", host: "github.com", port: 22},
		{raw: "GitHub.COM.", host: "github.com"},
		{raw: "github.com/*/x*", host: "github.com", url: true},
		{raw: ".githubusercontent.com/canonical/*", host: "githubusercontent.com", suffix: true, url: true},
		{raw: "api.github.com ~^/repos/[^/]+/x$", host: "api.github.com", url: true},
		{raw: "GET,HEAD github.com/*", host: "github.com", url: true, methods: []string{"GET", "HEAD"}},
		{raw: "POST api.github.com ~^/graphql$", host: "api.github.com", url: true, methods: []string{"POST"}},
		{raw: "10.0.5.20:5432", ip: "10.0.5.20", port: 5432},
		{raw: "10.0.5.20", ip: "10.0.5.20"},
		{raw: "::1", ip: "::1"},
		{raw: "[::1]:443", ip: "::1", port: 443},
		{raw: "fe80::1%ETH0", ip: "fe80::1"},
		{raw: "b\u00fccher.example", host: "xn--bcher-kva.example"},
	}
	for _, c := range ok {
		e, err := ParseEntry(c.raw, false)
		require.NoError(t, err, "%q", c.raw)
		assert.Equal(t, e.host, c.host, "%q host", c.raw)
		assert.Equal(t, e.suffix, c.suffix, "%q suffix", c.raw)
		assert.Equal(t, e.URL(), c.url, "%q url", c.raw)
		assert.Equal(t, e.port, c.port, "%q port", c.raw)
		assert.EqualArrays(t, e.Methods(), c.methods)
		if c.ip != "" {
			assert.Equal(t, e.ip, netip.MustParseAddr(c.ip), "%q ip", c.raw)
		}
	}

	bad := []string{
		"",
		"*",
		"*/chisel*",
		"/x",
		"*.github.com*",
		"a.*.b",
		"GET github.com",      // a method needs a url entry
		"github.com ~",        // empty regexp
		"github.com ~(",       // bad regexp
		"github.com:0",        // bad port
		"github.com:99999",    // bad port
		"github.com:x",        // bad port
		"github.com evil.com", // whitespace
		"[::1]",               // bracket form needs a port
		"a:b:c",               // not host:port
		"github.com..",        // an empty label: it could match no request
		"..github.com",
		"a..b",
		"xn--",            // a name that is nothing once decoded
		"github.com/\xff", // a glob that is no regexp
	}
	for _, raw := range bad {
		_, err := ParseEntry(raw, false)
		assert.Error(t, err, assert.AnyError, "%q", raw)
	}
}

func TestDecideHost(t *testing.T) {
	deny := mustRule(t, false, []string{"api.anthropic.com", ".ubuntu.com", "*.npmjs.org", "github.com:22", "10.0.0.5"}, nil)
	allow := mustRule(t, true, nil, []string{"github.com", ".githubusercontent.com", "evil.test/*"})

	cases := []struct {
		r    Rule
		host string
		port uint16
		want bool
	}{
		{deny, "api.anthropic.com", 443, true},  // exact allow
		{deny, "API.Anthropic.com.", 443, true}, // normalised
		{deny, "archive.ubuntu.com", 80, true},  // suffix allow
		{deny, "ubuntu.com", 443, true},         // bare suffix root
		{deny, "registry.npmjs.org", 443, true}, // *.suffix
		{deny, "notubuntu.com", 443, false},     // suffix needs a dot boundary
		{deny, "github.com", 443, false},        // only port 22 is allowed
		{deny, "github.com", 22, true},
		{deny, "api.anthropic.com", 8443, false}, // no port given: 80 and 443 only
		{deny, "10.0.0.5", 443, true},
		{deny, "evil.com", 443, false},                   // default deny
		{allow, "example.com", 443, true},                // default allow
		{allow, "github.com", 443, false},                // exact deny
		{allow, "raw.githubusercontent.com", 443, false}, // suffix deny
		{allow, "evil.test", 443, true},                  // a url deny cannot decide at the host stage
	}
	for _, c := range cases {
		got := c.r.DecideHost(c.host, c.port)
		assert.Equal(t, got.Allowed, c.want, "%s:%d", c.host, c.port)
	}
	assert.Equal(t, deny.DecideHost("api.anthropic.com", 443).Rule, "api.anthropic.com")
	assert.Equal(t, deny.DecideHost("evil.com", 443).Rule, "default")
	assert.Equal(t, deny.DecideHost("bad host", 443).Reason, "bad-host")
}

func TestDecideURL(t *testing.T) {
	r := mustRule(t, false,
		[]string{"github.com", "raw.githubusercontent.com", "docs.example.com/public**", "docs.example.com/api**"},
		[]string{"github.com/*/chisel-releases**", "raw.githubusercontent.com/canonical/chisel-releases**", "docs.example.com/internal**"})

	cases := []struct {
		host, path string
		want       bool
	}{
		{"github.com", "/canonical/chisel-releases", false},
		{"github.com", "/canonical/chisel-releases/tree/main", false},
		{"github.com", "/anyone/chisel-releases.git/info/refs", false},
		{"github.com", "/Canonical/Chisel-Releases", false}, // a deny matches case-insensitively
		{"github.com", "/canonical/chisel", true},
		{"github.com", "/canonical/other-repo", true},
		{"raw.githubusercontent.com", "/canonical/chisel-releases/main/x.yaml", false},
		{"raw.githubusercontent.com", "/other/chisel-releases/main/x.yaml", true}, // org anchored
		{"docs.example.com", "/public/guide", true},                               // allow match
		{"docs.example.com", "/Public/guide", false},                              // an allow matches exactly
		{"docs.example.com", "/api/v1", true},
		{"docs.example.com", "/private/x", false},       // restricted host, no allow match
		{"docs.example.com", "/internal/public", false}, // deny wins
		// a single * stays in its segment: the owner is pinned, a deeper repo of
		// the same name is not the answer repo.
		{"github.com", "/canonical/chisel-docs/blob/chisel-releases", true},
		{"github.com", "/a/b/chisel-releases", true},
	}
	for _, c := range cases {
		got := r.Decide(c.host, 443, "GET", c.path, "")
		assert.Equal(t, got.Allowed, c.want, "%s%s", c.host, c.path)
	}
	v := r.Decide("docs.example.com", 443, "GET", "/private/x", "")
	assert.Equal(t, v.Reason, "url-not-allowed")
	assert.Equal(t, v.Rule, "docs.example.com/public**") // the host's first allow entry, never "default"

	// the mitm set is every host a url entry names, and only those.
	assert.That(t, r.MitmHost("github.com", 443))
	assert.That(t, r.MitmHost("docs.example.com", 443))
	assert.That(t, !r.MitmHost("example.com", 443))
}

func TestGlobRe(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"/*", "/a", true},
		{"/*", "/a/b", false},
		{"/**", "/a/b", true},
		{"/a/*/c", "/a/b/c", true},
		{"/a/*/c", "/a/b/x/c", false},
		{"/a/**/c", "/a/b/x/c", true},
		{"/repo**", "/repo.git/info/refs", true},
		{"/repo*", "/repo.git", true},
		{"/repo*", "/repo/x", false},
		{"/a.b", "/axb", false}, // the rest is literal
		{"/***", "/a/b", true},
	} {
		re, err := globRe(c.glob, false)
		require.NoError(t, err)
		assert.Equal(t, re.MatchString(c.path), c.want, "%s ~ %s", c.glob, c.path)
	}
}

// the three candidate forms beyond the bare path: a deny anchored on the
// query, an allow that needs a specific query, and a ".." that path.Clean
// folds back onto a denied path.
func TestDecideQueryAndClean(t *testing.T) {
	t.Run("query deny", func(t *testing.T) {
		r := mustRule(t, true, nil, []string{"example.com/search?token=secret*"})
		cases := []struct {
			path, query string
			want        bool
		}{
			{"/search", "token=secret", false},
			{"/search", "token=public", true},
			{"/search", "", true},
		}
		for _, c := range cases {
			assert.Equal(t, r.Decide("example.com", 443, "GET", c.path, c.query).Allowed, c.want, "%s?%s", c.path, c.query)
		}
	})
	t.Run("query-only allow", func(t *testing.T) {
		r := mustRule(t, false, []string{"docs.example.com/api?ver=2*"}, nil)
		cases := []struct {
			path, query string
			want        bool
		}{
			{"/api", "ver=2", true},
			{"/api", "", false},
			{"/api", "ver=3", false},
		}
		for _, c := range cases {
			assert.Equal(t, r.Decide("docs.example.com", 443, "GET", c.path, c.query).Allowed, c.want, "%s?%s", c.path, c.query)
		}
	})
	t.Run("dot-dot deny", func(t *testing.T) {
		r := mustRule(t, true, nil, []string{"example.com/b*", "example.com/private/**"})
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/a/../b", "").Allowed, false)
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/a/../c", "").Allowed, true)
		// a trailing dot segment names the directory, slash and all.
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/private/x/..", "").Allowed, false)
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/private/.", "").Allowed, false)
		// nor do a segment's ;parameters or a backslash, as the origins that
		// drop the one and take the other for a slash read them.
		for _, p := range []string{"/private;x/key", "/public/..;/private/key", `/public/..\private/key`, `\private\key`, "/private;/key;v=2"} {
			assert.Equal(t, r.Decide("example.com", 443, "GET", p, "").Allowed, false, p)
		}
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/public;private/key", "").Allowed, true)
	})
}

// an allow errs the other way to a deny: it admits a path only when every
// reading of it matches, and a query only through a "?" the entry spells.
func TestDecideAllowReadings(t *testing.T) {
	r := mustRule(t, false, []string{
		"docs.example.com/public/**",
		"api.github.com ~^/repos/[^/]+/x$",
		"github.com/*/repo",
		"loose.test ~/public/",
		"search.test ~^/search\\?q=[a-z]+$",
		"class.test ~^/s[?&]x=1$",
		"next.test/login?next=**",
		"next.test ~^/sso\\?next=.*$",
		"count.test ~^/x/[^/]{3}$",
		"ext.test/x/*.json",
	}, nil)
	for _, c := range []struct {
		host, path, query string
		want              bool
	}{
		{"docs.example.com", "/public/x", "", true},
		{"docs.example.com", "/public/", "", true},
		{"docs.example.com", "/public/x", "a=b", true},
		{"docs.example.com", "/public/a/../b", "", true}, // both readings are under /public/
		{"docs.example.com", "/public/./x", "", true},
		{"docs.example.com", "/public//x", "", true},
		{"docs.example.com", "/public/../private", "", false},
		{"docs.example.com", "/public/..", "", false},
		{"docs.example.com", "/public/x/../../private", "", false},
		{"docs.example.com", "/private", "/public/x", false},
		// as an origin that drops a segment's ;parameters reads it, or one
		// that takes a backslash for a slash.
		{"docs.example.com", "/public/a;v=1/b", "", true},
		{"docs.example.com", "/public/..;/private", "", false},
		{"docs.example.com", "/public/..;x=1/..;/private", "", false},
		{"docs.example.com", `/public/..\private`, "", false},
		{"docs.example.com", `/public\..\private`, "", false},
		{"docs.example.com", `/public/a\b`, "", true},
		{"docs.example.com", `/public/..;\..\private`, "", false},
		{"ext.test", "/x/a.json", "", true},
		{"ext.test", "/x/secret;.json", "", false}, // /x/secret, to an origin that drops the parameter
		{"ext.test", `/x/a\b.json`, "", false},     // /x/a/b.json, a segment deeper than the entry goes

		{"api.github.com", "/repos/o/x", "", true},
		{"api.github.com", "/repos/o/x", "ref=main", true},
		{"api.github.com", "/repos/", "/x", false}, // the query is no segment
		{"api.github.com", "/repos/o", "/x", false},
		{"api.github.com", "/repos/o?/x", "", true}, // a "?" in the path (%3F on the wire) is a character of its segment
		{"api.github.com", "/repos/?/x", "", true},
		{"count.test", "/x/?", "", false}, // and one character, not the three it is on the wire
		{"count.test", "/x/abc", "", true},
		{"github.com", "/o/repo", "", true},
		{"github.com", "/", "/repo", false},
		{"github.com", "/o//repo", "", false}, // an origin that merges slashes serves /o/repo: allowed. one that does not, does not

		{"loose.test", "/a/public/b", "", true},
		{"loose.test", "/x", "/public/", false}, // an unanchored match, but inside the query alone

		{"search.test", "/search", "q=abc", true},
		{"search.test", "/search", "", false},
		{"search.test", "/search", "q=abc?x", false},
		{"search.test", "/other", "/search?q=abc", false},
		{"class.test", "/s", "x=1", true},

		{"next.test", "/login", "next=/home", true},
		{"next.test", "/login", "next=/a?b=1", true}, // a ? inside the query is a character of it
		{"next.test", "/sso", "next=/a?b=1&c=2", true},
		{"next.test", "/login/x", "next=1", false},
		{"next.test", "/", "/login?next=1", false},
		{"next.test", "/other", "x=1?next=2", false},
	} {
		got := r.Decide(c.host, 443, "GET", c.path, c.query).Allowed
		assert.Equal(t, got, c.want, "%s %s?%s", c.host, c.path, c.query)
	}
}

// spelledQuery changes what matches the join of a path and its query, and
// nothing else: text without the join matches as it did, a spelled "?" takes
// the join where it took a "?", and no wildcard does.
func TestSpelledQuery(t *testing.T) {
	join := string(querySep)
	for _, pattern := range []string{
		`^/repos/[^/]+/x$`, `.*`, `(?s)^/a.b$`, `^/[[:punct:]]+$`, `\S+`, `^/x[^a-z]*$`, `^/s[?&]x$`, `^/q\?a=.*$`, `^/[>-@]$`, `^/a|/b.c$`,
		`(?i)^/Q\?A=[^/]*$`, `^/a\??b$`, `^/(?:x\?y|z)+$`,
	} {
		re := regexp.MustCompile(pattern)
		strict, err := spelledQuery(re)
		require.NoError(t, err, pattern)
		for _, text := range []string{"/repos/o/x", "/a\nb", "/a?b", "/!!", "/x?", "/s?x", "/q?a=1", "/q?a=1?2", "/?", "/b?c", "/repos/?/x", "", "/Q?a=x", "/ab", "/x?yz"} {
			assert.Equal(t, strict.MatchString(text), re.MatchString(text), "%s on %q", pattern, text)
			if i := strings.Index(text, "?"); i >= 0 && strict.MatchString(text[:i]+join+text[i+1:]) {
				assert.That(t, re.MatchString(text), "%s takes %q with its first ? as the join, and not as it is", pattern, text)
			}
		}
	}
	for _, c := range []struct {
		pattern, text string
		want          bool
	}{
		{`^/q\?a=[^/]*$`, "/q" + join + "a=1", true},
		{`^/q\?a=[^/]*$`, "/q" + join + "a=1?2", true},       // past the join a ? is a character
		{`^/q\?a=[^/]*$`, "/q?a=1" + join + "2", false},      // and the join is not
		{`^/repos/[^/]+/x$`, "/repos/" + join + "/x", false}, // no wildcard takes the join
		{`^/repos/.+/x$`, "/repos/" + join + "/x", false},
		{`(?s)^/repos/.+/x$`, "/repos/" + join + "/x", false},
		{`^/s[?&]x$`, "/s" + join + "x", true}, // a class that lists ? spells it
		{`^/s[>-@]x$`, "/s" + join + "x", false},
		{`^/a\S*$`, "/a" + join + "b", false},
		{`^/a\PLb$`, "/a" + join + "b", false},
		{"^/a" + join + "b$", "/a" + join + "b", false}, // nor does the rune itself, written into an entry
		{`^/a[\x{E000}]b$`, "/a" + join + "b", false},
	} {
		strict, err := spelledQuery(regexp.MustCompile(c.pattern))
		require.NoError(t, err)
		assert.Equal(t, strict.MatchString(c.text), c.want, "%s on %q", c.pattern, c.text)
	}
}

// COVER: path.Clean("") returns "." (it differs from ""), so without a guard
// urlCandidates would add a bogus "." candidate for every request with no
// path at all.
func TestURLCandidatesEmptyPath(t *testing.T) {
	assert.EqualArrays(t, urlCandidates("", ""), []string{""})
	assert.EqualArrays(t, urlCandidates("", "q=1"), []string{"", "?q=1"})
	assert.EqualArrays(t, urlCandidates("/b", ""), []string{"/b"})
	assert.EqualArrays(t, urlCandidates("/a/../b", "q"), []string{"/a/../b", "/a/../b?q", "/b", "/b?q"})
	assert.EqualArrays(t, urlCandidates("/a/../b", ""), []string{"/a/../b", "/b"})
	assert.EqualArrays(t, urlCandidates("/a/x/..", ""), []string{"/a/x/..", "/a", "/a/"})
	assert.EqualArrays(t, urlCandidates("/a/..;/b", ""), []string{"/a/..;/b", "/a/../b", "/b"})
	assert.EqualArrays(t, urlCandidates(`/a\b`, "q"), []string{`/a\b`, `/a\b?q`, "/a/b", "/a/b?q"})
}

func TestDecideMethodsAndRegexp(t *testing.T) {
	r := mustRule(t, false,
		[]string{"GET,HEAD github.com/*", "POST api.github.com ~^/graphql$", "api.github.com ~^/repos/[^/]+/x$"},
		[]string{"PUT,PATCH gist.github.com/*", "api.github.com ~^/Secret"})

	cases := []struct {
		host, method, path string
		want               bool
	}{
		{"github.com", "GET", "/x", true},
		{"github.com", "POST", "/x", false}, // not in the entry's methods
		{"api.github.com", "POST", "/graphql", true},
		{"api.github.com", "GET", "/graphql", false},
		{"api.github.com", "GET", "/repos/canonical/x", true},
		{"api.github.com", "GET", "/repos/canonical/x/y", false}, // anchored
		{"api.github.com", "GET", "/secret/x", false},            // deny regexp, case-insensitive
		{"gist.github.com", "GET", "/x", false},                  // default deny, deny entry is method-scoped
	}
	for _, c := range cases {
		got := r.Decide(c.host, 443, c.method, c.path, "")
		assert.Equal(t, got.Allowed, c.want, "%s %s%s", c.method, c.host, c.path)
	}

	allow := mustRule(t, true, nil, []string{"PUT,PATCH gist.github.com/*"})
	assert.Equal(t, allow.Decide("gist.github.com", 443, "PUT", "/x", "").Allowed, false)
	assert.Equal(t, allow.Decide("gist.github.com", 443, "GET", "/x", "").Allowed, true)
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"GitHub.com":          "github.com",
		"github.com.":         "github.com",
		"b\u00fccher.example": "xn--bcher-kva.example",
		"[::1]":               "::1",
		"::FFFF:1.2.3.4":      "1.2.3.4",
	}
	cases["github\uff0ecom\u3002"] = "github.com" // idna's dots are dots, the root's too
	cases["1.2.3.4."] = "1.2.3.4"
	cases[" github.com "] = "github.com"
	for in, want := range cases {
		got, err := NormalizeHost(in)
		require.NoError(t, err, in)
		assert.Equal(t, got, want, in)
		again, err := NormalizeHost(got)
		require.NoError(t, err, got)
		assert.Equal(t, again, got, "%q is not normalised in one go", in)
	}
	// no host: nothing, and a name with an empty label, which stripping dots
	// until it parses would turn into another name.
	for _, in := range []string{"", ".", "..", "github.com..", ".github.com", "a..b", "xn--", "github.com\u3002."} {
		_, err := NormalizeHost(in)
		assert.Error(t, err, assert.AnyError, "%q", in)
	}
}

// a host that is no host is refused whatever the default: under default
// allow a deny on a name is not dodged by a second trailing dot.
func TestDecideBadHost(t *testing.T) {
	r := mustRule(t, true, nil, []string{"evil.test"})
	for _, host := range []string{"evil.test..", ".evil.test", "evil..test", "xn--"} {
		v := r.DecideHost(host, 443)
		assert.That(t, !v.Allowed && v.Reason == "bad-host", "%q: %+v", host, v)
		v = r.Decide(host, 443, "GET", "/", "")
		assert.That(t, !v.Allowed && v.Reason == "bad-host", "%q: %+v", host, v)
	}
	assert.That(t, !r.DecideHost("evil.test.", 443).Allowed, "the root's dot dodged the deny")
	zoned := mustRule(t, false, []string{"fe80::1%ETH0"}, nil)
	assert.That(t, zoned.DecideHost("fe80::1%eth0", 443).Allowed, "an address entry does not admit its address under a zone")
	assert.That(t, r.DecideHost("fine.test.", 443).Allowed, "a name with the root's dot is refused")
}

func TestPolicyRule(t *testing.T) {
	_, err := Policy{Default: "maybe"}.Rule()
	assert.Error(t, err, "default")
	_, err = Policy{DenyResponse: "500"}.Rule()
	assert.Error(t, err, "deny-response")
	_, err = Policy{Allow: []string{"*"}}.Rule()
	assert.Error(t, err, `allow "*"`)
	_, err = Policy{Methods: []string{}}.Rule()
	assert.Error(t, err, "empty list")
	_, err = Policy{Methods: []string{"get"}}.Rule()
	assert.Error(t, err, "upper-case")
	_, err = Policy{Harness: []string{"*"}}.Rule()
	assert.Error(t, err, "harness")
	assert.That(t, Policy{Methods: []string{"GET"}}.NeedsCA()) // a posture inspects
	assert.That(t, Policy{Deny: []string{"a.test/x*"}}.NeedsCA())
	assert.That(t, !Policy{Allow: []string{"a.test"}}.NeedsCA())
}

func TestGuard(t *testing.T) {
	pool := netip.MustParsePrefix("10.213.0.0/16")
	strict := &Guard{Refuse: []netip.Prefix{pool}}
	loose := &Guard{AllowPrivate: true, Refuse: []netip.Prefix{pool}}

	cases := []struct {
		addr     string
		strictOK bool
		looseOK  bool
		reason   string
	}{
		{"1.1.1.1", true, true, ""},
		{"2606:4700::1111", true, true, ""},
		{"127.0.0.1", false, true, "private"},
		{"::1", false, true, "private"},
		{"10.0.0.5", false, true, "private"},
		{"172.17.0.1", false, true, "private"},
		{"192.168.1.1", false, true, "private"},
		{"169.254.169.254", false, true, "private"},
		{"fe80::1", false, true, "private"},
		{"fd00::1", false, true, "private"},
		{"::ffff:10.0.0.5", false, true, "private"},
		{"224.0.0.1", false, true, "private"},
		{"100.100.1.1", false, true, "private"}, // cgnat: tailscale and the like
		{"::ffff:100.64.0.1", false, true, "private"},
		{"0.0.0.0", false, false, "unspecified"},
		{"::", false, false, "unspecified"},
		{"10.213.4.2", false, false, "refused-range"},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.addr)
		err := strict.check(a)
		assert.Equal(t, err == nil, c.strictOK, "strict %s", c.addr)
		if err != nil {
			assert.Equal(t, err.(*RefusedError).Reason, c.reason, c.addr)
		}
		assert.Equal(t, loose.check(a) == nil, c.looseOK, "loose %s", c.addr)
	}
	assert.Equal(t, dialReason(&RefusedError{Reason: "private"}), "private")
}

// FuzzDecide: host, path and query come straight from the client's request,
// so they're attacker-controlled. no panic across rule shapes, and two things
// every input must keep: a request the full verdict passes is on a host the
// tunnel verdict passed (the tunnel is never the stricter gate), and a deny
// that takes a path takes every spelling of it with dot segments.
func FuzzDecide(f *testing.F) {
	mixed := mustRule(f, false,
		[]string{"docs.example.com/public*", "api.github.com ~^/repos/[^/]+/x$", "GET github.com/*"},
		[]string{"github.com/*/chisel-releases*"})
	// default allow and deny entries alone: every refusal is a deny's.
	denies := mustRule(f, true, nil,
		[]string{"github.com/*/chisel-releases*", "docs.example.com/private/**", "example.com/search?token=*", "evil.test"})
	f.Add("github.com", "/canonical/chisel-releases/tree/main", "")
	f.Add("docs.example.com", "/public/guide", "ver=2")
	f.Add("", "", "")
	f.Add("*", "/*", "*")
	f.Add("github.com", "/../../etc/passwd", "")
	f.Add("docs.example.com", "/public/../private/key", "")
	f.Add("GITHUB.com.", "/x/../canonical/./chisel-releases", "a=b")
	f.Add("example.com", "/search", "token=abc")
	f.Fuzz(func(t *testing.T, host, urlPath, rawQuery string) {
		for _, r := range []Rule{mixed, denies} {
			v := r.Decide(host, 443, "GET", urlPath, rawQuery)
			if v.Allowed {
				assert.That(t, r.DecideHost(host, 443).Allowed,
					"%q %q passes the request verdict on a host the tunnel verdict refuses", host, urlPath)
			}
		}
		if urlPath == "" {
			return
		}
		for _, read := range []string{pathpkg.Clean(urlPath), resolvePath(urlPath)} {
			if c := denies.Decide(host, 443, "GET", read, rawQuery); !c.Allowed {
				assert.That(t, !denies.Decide(host, 443, "GET", urlPath, rawQuery).Allowed,
					"%q dodges the deny %q that takes it as %q", urlPath, c.Rule, read)
			}
		}
	})
}

// FuzzDecideAllow: a url allow admits paths, and an origin serves the path it
// resolves, not the one spelled. so a path that passes still passes resolved
// ("/public/../private" does not pass for "/public/**"), and still passes
// without its query: no entry here names one, so a query cannot stand in for
// the part of the path an entry asks for ("/repos/?/x" for "/repos/*/x").
func FuzzDecideAllow(f *testing.F) {
	r := mustRule(f, false,
		[]string{"docs.example.com/public/**", "api.github.com ~^/repos/[^/]+/x$", "GET github.com/*"}, nil)
	f.Add("docs.example.com", "/public/guide", "ver=2")
	f.Add("docs.example.com", "/public/a/../guide", "")
	f.Add("docs.example.com", "/public/", "")
	f.Add("github.com", "/canonical", "")
	f.Add("api.github.com", "/repos/o/x", "a=b")
	allowed := func(host, urlPath, rawQuery string) bool {
		return r.Decide(host, 443, "GET", urlPath, rawQuery).Allowed
	}
	f.Fuzz(func(t *testing.T, host, urlPath, rawQuery string) {
		if urlPath == "" || !allowed(host, urlPath, rawQuery) {
			return
		}
		assert.That(t, allowed(host, urlPath, ""),
			"%q is allowed only with the query %q", urlPath, rawQuery)
		assert.That(t, allowed(host, resolvePath(urlPath), rawQuery),
			"%q is allowed, but it resolves to %q, which is not", urlPath, resolvePath(urlPath))
	})
}

// FuzzSpelledQuery: the rewrite an allow's matcher goes through changes what
// matches the join of a path and its query, and nothing else. text without
// the join matches as it did, and with its first "?" made the join it matches
// no more than it did.
func FuzzSpelledQuery(f *testing.F) {
	f.Add(`^/repos/[^/]+/x$`, "/repos/o/x")
	f.Add(`(?s)^/a.b\?c=.*$`, "/a\nb?c=1")
	f.Add(`^/[[:punct:]]+$|\S+`, "/?!")
	f.Add(`^/s[?&]x$`, "/s?x")
	f.Add(`(?i)^/[^a-z]{2,3}?$`, "/A?")
	f.Add(`^/q\??a=(x|\?)*$`, "/q?a=x?")
	f.Fuzz(func(t *testing.T, pattern, text string) {
		re, err := regexp.Compile(pattern)
		if err != nil || strings.ContainsRune(pattern, querySep) || strings.ContainsRune(text, querySep) {
			return
		}
		strict, err := spelledQuery(re)
		require.NoError(t, err, "%q compiles, its rewrite does not", pattern)
		plain := re.MatchString(text)
		assert.Equal(t, strict.MatchString(text), plain, "%q on %q", pattern, text)
		if i := strings.Index(text, "?"); i >= 0 && strict.MatchString(text[:i]+string(querySep)+text[i+1:]) {
			assert.That(t, plain, "%q takes %q with its first ? as the join, and not as it is", pattern, text)
		}
	})
}

// FuzzNormalizeHost: every verdict is on the normalised host, so it has to be
// one form per name: normalising it again changes nothing, and what comes
// out is a lower-case name with no empty label and no dot left to strip.
func FuzzNormalizeHost(f *testing.F) {
	for _, s := range []string{"example.com", "EXAMPLE.com.", "b\u00fccher.example", "[::1]", "::ffff:1.2.3.4", "1.2.3.4", " a.test ", "a_b.test", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		n, err := NormalizeHost(raw)
		if err != nil {
			return
		}
		again, err := NormalizeHost(n)
		assert.NoError(t, err, "%q normalises to %q, which does not normalise", raw, n)
		assert.Equal(t, again, n, "%q is not normalised in one go", raw)
		assert.That(t, n != "", "%q normalises to no host", raw)
		assert.Equal(t, strings.ToLower(n), n)
		if _, err := netip.ParseAddr(n); err == nil {
			return
		}
		for _, label := range strings.Split(n, ".") {
			assert.That(t, label != "", "%q normalises to %q, with an empty label", raw, n)
		}
	})
}

// FuzzParseEntry: entries come from pats.yaml, but a hand-set policy can also
// feed them, so the parser must never panic. an entry it takes parses the
// same from its own Raw, and a host entry admits the host it names.
func FuzzParseEntry(f *testing.F) {
	for _, s := range []string{"github.com/*/chisel-releases*", "*/x", "", "a/b/c*d*e", "GET a.b ~^/x", "[::1]:443", "a:b:c",
		".example.com", "*.example.com:8443", "1.2.3.4", "GET,POST a.b/**", "a.b ~(", " a.b "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		for _, deny := range []bool{false, true} {
			e, err := ParseEntry(raw, deny)
			if err != nil {
				continue
			}
			again, err := ParseEntry(e.Raw, deny)
			assert.NoError(t, err, "%q parses, its Raw %q does not", raw, e.Raw)
			assert.Equal(t, again.host, e.host)
			assert.Equal(t, again.suffix, e.suffix)
			assert.Equal(t, again.port, e.port)
			assert.Equal(t, again.ip, e.ip)
			if deny || e.URL() {
				continue
			}
			host, port := e.host, e.port
			if e.ip.IsValid() {
				host = e.ip.String()
			}
			if port == 0 {
				port = 443
			}
			r := Rule{Allow: []Entry{e}}
			assert.That(t, r.DecideHost(host, port).Allowed, "the entry %q does not admit its own host %q", raw, host)
			if e.suffix {
				assert.That(t, r.DecideHost("sub."+host, port).Allowed, "the suffix entry %q does not admit a subdomain", raw)
			}
		}
	})
}

// BenchmarkDecide pins the per-request filtering overhead at a deliberately
// heavy entry count (50 url entries; real configs have a handful).
func BenchmarkDecide(b *testing.B) {
	var deny, allow []string
	for i := range 25 {
		deny = append(deny, "host"+string(rune('a'+i))+".example.com/*/secrets*")
		allow = append(allow, "docs"+string(rune('a'+i))+".example.com/public*")
	}
	r, err := ParseRule(false, allow, deny)
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct{ host, path string }{
		{"hosty.example.com", "/x/secrets/key.pem"},
		{"docsy.example.com", "/public/guide"},
		{"other.example.com", "/anything"},
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		c := cases[i%len(cases)]
		r.Decide(c.host, 443, "GET", c.path, "")
	}
}

// BenchmarkDecideHost is the verdict every CONNECT gets before a byte moves:
// the host normalised and walked down both lists, here 50 entries long with
// ip entries among them.
func BenchmarkDecideHost(b *testing.B) {
	var deny, allow []string
	for i := range 25 {
		deny = append(deny, fmt.Sprintf("203.0.113.%d", i))
		allow = append(allow, fmt.Sprintf(".docs%d.example.com", i))
	}
	r := mustRule(b, false, allow, deny)
	hosts := []string{"api.docs24.example.com", "Other.Example.com.", "203.0.113.24", "198.51.100.7"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		r.DecideHost(hosts[i%len(hosts)], 443)
	}
}

// the methods posture: unlisted methods are refused on every inspected host;
// an allow entry's own method prefix overrides it; a deny entry's method
// prefix scopes the deny; harness hosts are not inspected for it, but a request
// seen on one anyway is held to it.
func TestMethodsPosture(t *testing.T) {
	p := Policy{
		Methods: []string{"GET", "HEAD"},
		Allow:   []string{"a.test", "POST a.test/graphql", ".anthropic.com"},
		Deny:    []string{"PUT,PATCH b.test/*"},
		Harness: []string{".anthropic.com"},
	}
	r, err := p.Rule()
	require.NoError(t, err)
	cases := []struct {
		host, method, path string
		want               bool
		reason             string
	}{
		{"a.test", "GET", "/x", true, ""},
		{"a.test", "HEAD", "/x", true, ""},
		{"a.test", "POST", "/x", false, "method"},
		{"a.test", "POST", "/graphql", true, ""}, // the entry's own prefix wins
		{"a.test", "DELETE", "/graphql", false, "method"},
		{"api.anthropic.com", "GET", "/v1/models", true, ""},
		{"c.test", "GET", "/", false, ""}, // default deny still applies
	}
	for _, c := range cases {
		v := r.Decide(c.host, 443, c.method, c.path, "")
		assert.Equal(t, v.Allowed, c.want, "%s %s%s", c.method, c.host, c.path)
		assert.Equal(t, v.Reason, c.reason, "%s %s%s", c.method, c.host, c.path)
	}

	// every allowed host but the exempt is inspected.
	assert.That(t, r.MitmHost("a.test", 443))
	assert.That(t, !r.MitmHost("api.anthropic.com", 443))

	// under default: allow the posture covers every host reached.
	p.Default = "allow"
	r, err = p.Rule()
	require.NoError(t, err)
	assert.That(t, r.MitmHost("anything.test", 443))
	assert.Equal(t, r.Decide("anything.test", 443, "POST", "/", "").Reason, "method")
	assert.Equal(t, r.Decide("b.test", 443, "PUT", "/x", "").Allowed, false)
	assert.Equal(t, r.Decide("b.test", 443, "GET", "/x", "").Allowed, true)

	// an explicit url entry on an exempt host still mitm's it, and the
	// posture applies to what that shows, as it does to plain http there.
	p.Deny = append(p.Deny, "api.anthropic.com/v1/files*")
	r, err = p.Rule()
	require.NoError(t, err)
	assert.That(t, r.MitmHost("api.anthropic.com", 443))
	assert.Equal(t, r.Decide("api.anthropic.com", 443, "POST", "/v1/messages", "").Reason, "method")
	assert.Equal(t, r.Decide("api.anthropic.com", 80, "POST", "/v1/messages", "").Reason, "method")
	assert.Equal(t, r.Decide("api.anthropic.com", 443, "GET", "/v1/models", "").Allowed, true)
}
