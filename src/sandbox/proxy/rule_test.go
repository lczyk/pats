package proxy

import (
	"net/netip"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func mustRule(t *testing.T, defaultAllow bool, allow, deny []string) Rule {
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
		[]string{"github.com", "raw.githubusercontent.com", "docs.example.com/public*", "docs.example.com/api*"},
		[]string{"github.com/*/chisel-releases*", "raw.githubusercontent.com/canonical/chisel-releases*", "docs.example.com/internal*"})

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
	}
	for _, c := range cases {
		got := r.Decide(c.host, 443, "GET", c.path, "")
		assert.Equal(t, got.Allowed, c.want, "%s%s", c.host, c.path)
	}
	v := r.Decide("docs.example.com", 443, "GET", "/private/x", "")
	assert.Equal(t, v.Reason, "url-not-allowed")
	assert.Equal(t, v.Rule, "docs.example.com/public*") // the host's first allow entry, never "default"

	// the mitm set is every host a url entry names, and only those.
	assert.That(t, r.MitmHost("github.com", 443))
	assert.That(t, r.MitmHost("docs.example.com", 443))
	assert.That(t, !r.MitmHost("example.com", 443))
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
		r := mustRule(t, true, nil, []string{"example.com/b*"})
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/a/../b", "").Allowed, false)
		assert.Equal(t, r.Decide("example.com", 443, "GET", "/a/../c", "").Allowed, true)
	})
}

// COVER: path.Clean("") returns "." (it differs from ""), so without a guard
// urlCandidates would add a bogus "." candidate for every request with no
// path at all.
func TestURLCandidatesEmptyPath(t *testing.T) {
	assert.EqualArrays(t, urlCandidates("", ""), []string{""})
	assert.EqualArrays(t, urlCandidates("", "q=1"), []string{"", "?q=1"})
	assert.EqualArrays(t, urlCandidates("/b", ""), []string{"/b"})
	assert.EqualArrays(t, urlCandidates("/a/../b", "q"), []string{"/a/../b", "/a/../b?q", "/b", "/b?q"})
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
	for in, want := range cases {
		got, err := NormalizeHost(in)
		require.NoError(t, err, in)
		assert.Equal(t, got, want, in)
	}
	_, err := NormalizeHost("")
	assert.Error(t, err, assert.AnyError)
}

func TestPolicyRule(t *testing.T) {
	_, err := Policy{Default: "maybe"}.Rule()
	assert.Error(t, err, "default")
	_, err = Policy{DenyResponse: "500"}.Rule()
	assert.Error(t, err, "deny-response")
	_, err = Policy{Allow: []string{"*"}}.Rule()
	assert.Error(t, err, `allow "*"`)
	_, err = Policy{Methods: []string{"GET"}}.Rule()
	assert.Error(t, err, "methods is not implemented yet")
	_, err = Policy{Allow: []string{"GET a.test/*"}}.Rule()
	assert.Error(t, err, "method prefix is not implemented yet")
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
// so they're attacker-controlled. just wants no panic across rule shapes.
func FuzzDecide(f *testing.F) {
	r, err := ParseRule(false,
		[]string{"docs.example.com/public*", "api.github.com ~^/repos/[^/]+/x$", "GET github.com/*"},
		[]string{"github.com/*/chisel-releases*"})
	if err != nil {
		f.Fatal(err)
	}
	f.Add("github.com", "/canonical/chisel-releases/tree/main", "")
	f.Add("docs.example.com", "/public/guide", "ver=2")
	f.Add("", "", "")
	f.Add("*", "/*", "*")
	f.Add("github.com", "/../../etc/passwd", "")
	f.Fuzz(func(t *testing.T, host, urlPath, rawQuery string) {
		r.Decide(host, 443, "GET", urlPath, rawQuery)
		r.DecideHost(host, 443)
	})
}

// FuzzParseEntry: entries come from pats.yaml, but a hand-set policy can also
// feed them, so the parser must never panic.
func FuzzParseEntry(f *testing.F) {
	for _, s := range []string{"github.com/*/chisel-releases*", "*/x", "", "a/b/c*d*e", "GET a.b ~^/x", "[::1]:443", "a:b:c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ParseEntry(raw, false)
		ParseEntry(raw, true)
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
