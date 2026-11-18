package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"gopkg.in/yaml.v3"
)

func rawRule(t testing.TB, defaultAllow bool, allow, deny []RawEntry) Rule {
	t.Helper()
	r, err := ParseRawRule(defaultAllow, allow, deny)
	require.NoError(t, err)
	return r
}

// but is an entry with exceptions.
func but(match string, except ...string) RawEntry { return RawEntry{Match: match, Except: except} }

// on asks r about a request to github.com over tls.
func on(r Rule, method, path, query string) Verdict {
	return r.Decide("github.com", 443, method, path, query)
}

const lsRemote = "GET /*/answers.git/info/refs?service=git-upload-pack"

// the case the exception was made for: a repository denied whole but for the
// one request a `git ls-remote` makes.
func TestDenyExcept(t *testing.T) {
	deny := []RawEntry{but("github.com/*/answers.git**", lsRemote)}
	r := rawRule(t, false, Lines("github.com"), deny)
	refs := "/x/answers.git/info/refs"
	for _, c := range []struct {
		method, path, query string
		rule                string
	}{
		{"GET", refs, "service=git-upload-pack", "github.com"},
		{"get", refs, "service=git-upload-pack", "github.com"}, // a method is matched whatever its case
		{"GET", refs, "service=git-receive-pack", deny[0].Match},
		{"GET", refs, "service=git-upload-pack&x=1", deny[0].Match},
		{"GET", refs, "", deny[0].Match},
		{"POST", refs, "service=git-upload-pack", deny[0].Match},
		{"POST", "/x/answers.git/git-upload-pack", "", deny[0].Match},
		{"GET", "/x/answers.git/INFO/refs", "service=git-upload-pack", deny[0].Match}, // the match folds case, the exception does not
		{"GET", "/x/other", "", "github.com"},
	} {
		v := on(r, c.method, c.path, c.query)
		assert.Equal(t, v.Rule, c.rule, "%s %s?%s", c.method, c.path, c.query)
		assert.Equal(t, v.Allowed, c.rule == "github.com", "%s %s?%s", c.method, c.path, c.query)
	}

	// an exception grants nothing: with no allow for the host the spared
	// request meets the default, and its CONNECT is refused before any path.
	closed := rawRule(t, false, nil, deny)
	v := on(closed, "GET", refs, "service=git-upload-pack")
	assert.Equal(t, v, Verdict{Rule: "default"})
	assert.Equal(t, closed.DecideHost("github.com", 443), Verdict{Rule: "default"})
	assert.That(t, closed.MitmHost("github.com", 443), "a mapping is a url entry: its host is terminated")
}

// an exception belongs to one entry: another deny still takes the request,
// and two entries with one match spare only what both do.
func TestDenyExceptIsTheEntrysOwn(t *testing.T) {
	spare := but("github.com/*/answers.git**", "/*/answers.git/info/refs")
	r := rawRule(t, false, Lines("github.com"), []RawEntry{spare, {Match: "github.com/*/answers.git/info/**"}})
	assert.Equal(t, on(r, "GET", "/x/answers.git/info/refs", "").Rule, "github.com/*/answers.git/info/**")

	r = rawRule(t, false, Lines("github.com"), []RawEntry{spare, but(spare.Match, "/*/answers.git/HEAD")})
	assert.Equal(t, on(r, "GET", "/x/answers.git/info/refs", "").Allowed, false)
	assert.Equal(t, on(r, "GET", "/x/answers.git/HEAD", "").Allowed, false)

	r = rawRule(t, false, Lines("github.com"), []RawEntry{spare, but(spare.Match, "/*/answers.git/HEAD", "/*/answers.git/info/refs")})
	assert.Equal(t, on(r, "GET", "/x/answers.git/info/refs", "").Allowed, true)
	assert.Equal(t, on(r, "GET", "/x/answers.git/HEAD", "").Allowed, false)
}

// an exception spares a path only when every reading of it matches: a path
// that an origin may read as one outside the exception is not spared.
func TestDenyExceptEveryReading(t *testing.T) {
	r := rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/*/answers.git**", "/*/answers.git/info/**")})
	for _, spared := range []string{"/x/answers.git/info/refs", "/x/answers.git/info/a/b", "/x/answers.git/info/a/../b"} {
		assert.That(t, on(r, "GET", spared, "").Allowed, "%q is refused", spared)
	}
	for _, taken := range []string{
		"/x/answers.git/info/../git-upload-pack",
		"/x/answers.git/info/a/../../git-upload-pack",
		"/x/answers.git/info/..;/git-upload-pack",
		"/x/answers.git/info/..;x=1/git-upload-pack",
		`/x/answers.git/info/..\git-upload-pack`,
		`/x/answers.git/info\..\git-upload-pack`,
		"/x/answers.git/info/%2e%2e/git-upload-pack", // still encoded once decoded: sent encoded twice
		"/x/answers.git/info/..%2fgit-upload-pack",
		"/x/answers.git/info//../../answers.git/git-upload-pack",
		"/x/answers.git/info/\uff0e\uff0e/git-upload-pack",
		"/x/answers.git/info/\u2024\u2024\uff0fgit-upload-pack",
		"/x/answers.git/info/%2e%2e;y\\..\uff0fgit-upload-pack",
		"/x/answers.git/info/a/..",  // resolves to info/, which an origin may serve as info
		"/x/answers.git/info/",      // the same, with nothing to resolve
		"/x/answers.git/info/a?b",   // a "?" of the path's own
		"/x/answers.git/info/a%3Fb", // and one a second decoding uncovers
		"/x/answers.git/info/a\x01", // no text: spared by nothing
		"/x/answers.git/info/\xff",
	} {
		v := on(r, "GET", taken, "")
		assert.Equal(t, v.Rule, "github.com/*/answers.git**", "%q", taken)
		assert.That(t, !v.Allowed, "%q is spared", taken)
	}

	// a regexp item may name what a reading rewrites: it spares the path
	// only where every reading of it still matches.
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/*/answers.git**", `~/[^/]+/answers\.git/info/[^/]+`)})
	assert.That(t, on(r, "GET", "/x/answers.git/info/refs;v=1", "").Allowed)
	assert.That(t, !on(r, "GET", "/x/answers.git/info/..;/x", "").Allowed)
}

// a regexp item is the whole path on a deny: unanchored it would spare every
// path that holds the text.
func TestDenyExceptRegexpIsAnchored(t *testing.T) {
	r := rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/**", `~/[^/]+/answers\.git/info/refs`)})
	assert.That(t, on(r, "GET", "/x/answers.git/info/refs", "").Allowed)
	for _, longer := range []string{"/x/answers.git/info/refs/more", "/pre/x/answers.git/info/refs", "/x/answers.git/info/refsx"} {
		assert.That(t, !on(r, "GET", longer, "").Allowed, "%q is spared", longer)
	}
	// an item that spells a query spares the path with that query, never a
	// path that holds the "?" itself.
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/**", "/x/refs?service=a")})
	assert.That(t, on(r, "GET", "/x/refs", "service=a").Allowed)
	assert.That(t, !on(r, "GET", "/x/refs?service=a", "").Allowed)
	assert.That(t, !on(r, "GET", "/x/refs?service=a", "service=a").Allowed)
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/**", `~/[^/]+/answers\.git/info/refs`)})
	// with no query spelled the path is spared with any, and a regexp that
	// spells one spares that one.
	assert.That(t, on(r, "GET", "/x/answers.git/info/refs", "service=x").Allowed)
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/**", `~/x/refs\?service=(a|b)`)})
	assert.That(t, on(r, "GET", "/x/refs", "service=a").Allowed)
	assert.That(t, !on(r, "GET", "/x/refs", "service=c").Allowed)
	assert.That(t, !on(r, "GET", "/x/refs", "").Allowed)
	// a regexp that asks for it folds case.
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/**", `~(?i)/x/refs`)})
	assert.That(t, on(r, "GET", "/X/Refs", "").Allowed)
}

// an allow's exception withholds as a deny takes: on any reading, in any
// case, and a regexp anywhere in the path.
func TestAllowExcept(t *testing.T) {
	allow := []RawEntry{but("github.com/canonical/**", "/canonical/answers**")}
	for _, defaultAllow := range []bool{false, true} {
		r := rawRule(t, defaultAllow, allow, nil)
		assert.Equal(t, on(r, "GET", "/canonical/chisel", ""), Verdict{Allowed: true, Rule: allow[0].Match})
		for _, withheld := range []string{
			"/canonical/answers",
			"/canonical/answers/x",
			"/canonical/ANSWERS",
			"/canonical/x/../answers",
			"/canonical/x/..;/answers",
			`/canonical/x\..\answers`,
			"/canonical/x%2f..%2fanswers",
			"/canonical/x/\uff0e\uff0e/answers/y",
		} {
			assert.Equal(t, on(r, "GET", withheld, ""), Verdict{Rule: allow[0].Match, Reason: "url-not-allowed"}, "%q", withheld)
		}
	}

	// it is not a deny: another allow on the host admits what it withholds.
	r := rawRule(t, false, append(allow, RawEntry{Match: "github.com/canonical/answers/README.md"}), nil)
	assert.Equal(t, on(r, "GET", "/canonical/answers/README.md", "").Rule, "github.com/canonical/answers/README.md")
	assert.Equal(t, on(r, "GET", "/canonical/answers/x", "").Allowed, false)
	r = rawRule(t, false, append(allow, RawEntry{Match: "github.com"}), nil)
	assert.Equal(t, on(r, "GET", "/canonical/answers/x", ""), Verdict{Allowed: true, Rule: "github.com"})
	// two entries with one match admit what either does.
	r = rawRule(t, false, append(allow, but(allow[0].Match, "/canonical/chisel**")), nil)
	assert.That(t, on(r, "GET", "/canonical/answers", "").Allowed)
	assert.That(t, on(r, "GET", "/canonical/chisel", "").Allowed)

	// a regexp item is unanchored, and a glob may write what a reading
	// rewrites: it is matched as written, on every reading.
	r = rawRule(t, false, []RawEntry{but("github.com/**", "~answers", "/a;b/**")}, nil)
	assert.That(t, !on(r, "GET", "/x/my-Answers-fork/y", "").Allowed)
	assert.That(t, !on(r, "GET", "/a;b/c", "").Allowed)
	assert.That(t, on(r, "GET", "/a/c", "").Allowed)
}

// a method prefix narrows: on the match the entry, on an item the exception.
func TestExceptMethods(t *testing.T) {
	r := rawRule(t, false, Lines("github.com"), []RawEntry{but("POST,PUT github.com/x/**", "POST /x/ok")})
	for method, want := range map[string]bool{"POST": true, "post": true, "PUT": false, "GET": true, "DELETE": true} {
		assert.Equal(t, on(r, method, "/x/ok", "").Allowed, want, method)
	}
	assert.Equal(t, on(r, "POST", "/x/other", "").Allowed, false)

	r = rawRule(t, false, []RawEntry{but("github.com/y/**", "POST,PUT /y/**")}, nil)
	assert.That(t, on(r, "GET", "/y/a", "").Allowed)
	assert.That(t, !on(r, "POST", "/y/a", "").Allowed)

	// an allow's own method passes a methods posture, and its exception
	// takes that back for what it names.
	r = rawRule(t, false, []RawEntry{but("POST github.com/z/**", "/z/no**")}, nil)
	r.Methods = map[string]bool{"GET": true}
	assert.Equal(t, on(r, "POST", "/z/a", ""), Verdict{Allowed: true, Rule: "POST github.com/z/**"})
	assert.Equal(t, on(r, "POST", "/z/no", ""), Verdict{Rule: "POST github.com/z/**", Reason: "url-not-allowed"})

	// a spared request is still held to the posture.
	r = rawRule(t, false, Lines("github.com"), []RawEntry{but("github.com/x/**", "/x/ok")})
	r.Methods = map[string]bool{"GET": true}
	assert.That(t, on(r, "GET", "/x/ok", "").Allowed)
	assert.Equal(t, on(r, "POST", "/x/ok", ""), Verdict{Rule: "methods", Reason: "method"})
}

// an item names no host: it applies wherever its entry does.
func TestExceptScope(t *testing.T) {
	r := rawRule(t, true, nil, []RawEntry{
		but(".example.com/**", "/pub/**"),
		but("203.0.113.7/**", "/pub/**"),
		but("ported.test:8443/**", "/pub/**"),
	})
	for _, c := range []struct {
		host string
		port uint16
	}{
		{"example.com", 443}, {"example.com", 80}, {"sub.example.com", 443}, {"a.b.example.com", 80},
		{"203.0.113.7", 443}, {"ported.test", 8443},
	} {
		assert.That(t, r.Decide(c.host, c.port, "GET", "/pub/x", "").Allowed, "%s:%d /pub/x", c.host, c.port)
		assert.That(t, !r.Decide(c.host, c.port, "GET", "/priv/x", "").Allowed, "%s:%d /priv/x", c.host, c.port)
		assert.That(t, r.MitmHost(c.host, c.port), "%s:%d is not terminated", c.host, c.port)
	}
	// where the entry does not apply, neither does anything of it.
	assert.That(t, r.Decide("ported.test", 443, "GET", "/priv/x", "").Allowed)
	assert.That(t, r.Decide("other.test", 443, "GET", "/priv/x", "").Allowed)
}

// an exception is read where a path is, and cannot open a host.
func TestExceptHostStage(t *testing.T) {
	spare := but("github.com/secret/**", "/secret/open")
	// a bare deny on the host refuses its CONNECT, whatever another entry spares.
	r := rawRule(t, true, nil, []RawEntry{spare, {Match: "github.com"}})
	assert.Equal(t, r.DecideHost("github.com", 443), Verdict{Rule: "github.com"})
	assert.That(t, !on(r, "GET", "/secret/open", "").Allowed)

	for _, defaultAllow := range []bool{false, true} {
		// a url allow restricts the host: a spared path is held to it too.
		r = rawRule(t, defaultAllow, Lines("github.com/secret/**"), []RawEntry{spare})
		assert.That(t, r.DecideHost("github.com", 443).Allowed)
		assert.That(t, on(r, "GET", "/secret/open", "").Allowed)
		assert.That(t, !on(r, "GET", "/secret/x", "").Allowed)
		r = rawRule(t, defaultAllow, Lines("github.com/other/**"), []RawEntry{spare})
		assert.Equal(t, on(r, "GET", "/secret/open", ""), Verdict{Rule: "github.com/other/**", Reason: "url-not-allowed"})
	}
	// under default: allow a spared path meets the default.
	r = rawRule(t, true, nil, []RawEntry{spare})
	assert.Equal(t, on(r, "GET", "/secret/open", ""), Verdict{Allowed: true, Rule: "default"})
	assert.Equal(t, on(r, "GET", "/secret/x", ""), Verdict{Rule: spare.Match})
	// plain http is decided the same way.
	assert.That(t, r.Decide("github.com", 80, "GET", "/secret/open", "").Allowed)
	assert.That(t, !r.Decide("github.com", 80, "GET", "/secret/x", "").Allowed)

	// a bound key's endpoint is an allow, and an allow never beats a deny:
	// under one it is reached only through that entry's own exception.
	bind := "POST openrouter.test/api/v1/chat/completions"
	r = rawRule(t, false, Lines(bind), Lines("openrouter.test/api/**"))
	assert.That(t, !r.Decide("openrouter.test", 443, "POST", "/api/v1/chat/completions", "").Allowed)
	r = rawRule(t, false, Lines(bind), []RawEntry{but("openrouter.test/api/**", "POST /api/v1/chat/completions")})
	assert.Equal(t, r.Decide("openrouter.test", 443, "POST", "/api/v1/chat/completions", ""), Verdict{Allowed: true, Rule: bind})
	assert.That(t, !r.Decide("openrouter.test", 443, "POST", "/api/v1/models", "").Allowed)
}

// DecideTrace names each entry that matched and left the request alone.
func TestDecideTrace(t *testing.T) {
	r := rawRule(t, false,
		[]RawEntry{but("github.com/canonical/**", "/canonical/answers**"), {Match: "github.com/canonical/answers/open"}},
		[]RawEntry{but("github.com/*/answers**", "/*/answers/open"), but("github.com/**", "/canonical/**")})
	v, left := r.DecideTrace("github.com", 443, "GET", "/canonical/answers/open", "")
	assert.Equal(t, v, Verdict{Allowed: true, Rule: "github.com/canonical/answers/open"})
	assert.EqualArrays(t, left, []Left{
		{Deny: true, Entry: "github.com/*/answers**", Item: "/*/answers/open"},
		{Deny: true, Entry: "github.com/**", Item: "/canonical/**"},
		{Entry: "github.com/canonical/**", Item: "/canonical/answers**"},
	})
	v, left = r.DecideTrace("github.com", 443, "GET", "/canonical/chisel", "")
	assert.Equal(t, v.Allowed, true)
	assert.Len(t, left, 1)
	_, left = r.DecideTrace("github.com", 443, "GET", "/other", "")
	assert.Len(t, left, 0)
}

func TestExceptLoadErrors(t *testing.T) {
	for _, c := range []struct {
		entry RawEntry
		deny  bool
		want  string
	}{
		{but("github.com", "/x"), true, "an except needs a url entry: write github.com/**"},
		{but("github.com", "/x"), false, "an except needs a url entry: write github.com/**"},
		{but("github.com/**", "github.com/x"), true, "with no host"},
		{but("github.com/**", "x"), true, "want a path glob"},
		{but("github.com/**", "GET"), true, "want a path glob"},
		{but("github.com/**", "get /x"), true, "want a path glob"},
		{but("github.com/**", "~"), true, "empty regexp"},
		{but("github.com/**", "~("), true, "regexp"},
		{but("github.com/**", "~("), false, "regexp"},
		{but("github.com/**", "/a;b"), true, "cannot write"},
		{but("github.com/**", `/a\b`), true, "cannot write"},
		{but("github.com/**", "/a%2fb"), true, "cannot write"},
		{but("github.com/**", "/a\uff0fb"), true, "cannot write"},
		{but("github.com/**", "/ok", "/a;b"), true, `except "/a;b"`},
	} {
		_, err := ParseRawEntry(c.entry, c.deny)
		assert.Error(t, err, c.want, "%v", c.entry)
	}
	// an allow's glob item may write any of them.
	for _, ok := range []string{"/a;b", `/a\b`, "/a%2fb", "/a\uff0fb"} {
		_, err := ParseRawEntry(but("github.com/**", ok), false)
		assert.NoError(t, err)
	}
	_, err := Policy{Deny: []RawEntry{but("github.com", "/x")}}.Rule()
	assert.Error(t, err, `deny "github.com": an except needs a url entry`)
}

// an entry is a line or a mapping of match and except, in yaml and in json
// alike, and nothing else.
func TestRawEntryDecode(t *testing.T) {
	var got struct {
		Deny []RawEntry `yaml:"deny" json:"deny"`
	}
	src := "deny:\n  - github.com\n  - match: 'github.com/x/**'\n    except: ['/x/ok', 'GET /x/refs?a=b']\n  - 10.0.5.20\n"
	require.NoError(t, yaml.Unmarshal([]byte(src), &got))
	want := []RawEntry{{Match: "github.com"}, but("github.com/x/**", "/x/ok", "GET /x/refs?a=b"), {Match: "10.0.5.20"}}
	assert.Len(t, got.Deny, 3)
	for i := range want {
		assert.That(t, got.Deny[i].Same(want[i]), "%d: %+v", i, got.Deny[i])
	}

	for src, wantErr := range map[string]string{
		"deny: [{match: 'a.test/**'}]":                                   "needs an except -- with none, write it as a line",
		"deny: [{match: 'a.test/**', except: }]":                         "want a list of one or more url parts",
		"deny: [{match: 'a.test/**', except: []}]":                       "want a list of one or more url parts",
		"deny: [{match: 'a.test/**', except: /x}]":                       "want a list of one or more url parts",
		"deny: [{match: 'a.test/**', except: [{a: b}]}]":                 "an item is a url part",
		"deny: [{match: 'a.test/**', except: [~]}]":                      "an item is a url part",
		"deny: [{match: 'a.test/**', except: ['']}]":                     "an item is a url part",
		"deny: [{except: [/x]}]":                                         "needs a match",
		"deny: [{match: , except: [/x]}]":                                "match: want an entry",
		"deny: [{match: [a], except: [/x]}]":                             "match: want an entry",
		"deny: [{match: 'a.test/**', except: [/x], when: 'True'}]":       `unknown field "when"`,
		"deny: [{match: 'a.test/**', except: [/x], name: n}]":            `unknown field "name"`,
		"deny: [[a.test]]":                                               "an entry is a line, or a mapping",
		"deny: [{match: 'a.test/**', match: 'b.test/**', except: [/x]}]": "match is written twice",
	} {
		assert.Error(t, yaml.Unmarshal([]byte(src), &got), wantErr, src)
	}

	for src, wantErr := range map[string]string{
		`{"deny":[{"match":"a.test/**"}]}`:                                 "needs a match and an except",
		`{"deny":[{"match":"a.test/**","except":[]}]}`:                     "needs a match and an except",
		`{"deny":[{"except":["/x"]}]}`:                                     "needs a match and an except",
		`{"deny":[{"match":"a.test/**","except":["/x"],"when":1}]}`:        "unknown field",
		`{"deny":[{"match":"a.test/**","except":["/x"],"except":["/y"]}]}`: "except is written twice",
		`{"deny":[{"Match":"a.test/**","except":["/x"]}]}`:                 `unknown field "Match"`,
		`{"deny":[{"match":"a.test/**","except":[""]}]}`:                   "needs a match and an except",
		`{"deny":[{"match":"a.test/**","except":"/x"}]}`:                   "an entry is a line, or a mapping",
		`{"deny":[{"match":"a.test/**","except":[1]}]}`:                    "an entry is a line, or a mapping",
		`{"deny":[["a.test"]]}`:                                            "an entry is a line, or a mapping",
		`{"deny":[7]}`:                                                     "an entry is a line, or a mapping",
	} {
		assert.Error(t, json.Unmarshal([]byte(src), &got), wantErr, src)
	}
}

// a policy's lists come back as they went out, through the yaml a proxy is
// handed and the json a run records, an entry with no exception a line in
// each.
func TestPolicyEntriesRoundTrip(t *testing.T) {
	in := Policy{
		Allow: []RawEntry{{Match: ".ubuntu.com"}, but("github.com/canonical/**", "/canonical/answers**")},
		Deny:  []RawEntry{but("github.com/*/answers.git**", lsRemote), {Match: "evil.test"}},
	}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	assert.ContainsString(t, string(y), "- .ubuntu.com\n")
	assert.ContainsString(t, string(y), "- match: github.com/canonical/**\n")
	assert.ContainsString(t, string(y), "- evil.test\n")
	fromYAML, err := ParsePolicy(y)
	require.NoError(t, err)

	j, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Equal(t, string(j), `{"allow":[".ubuntu.com",{"match":"github.com/canonical/**","except":["/canonical/answers**"]}],`+
		`"deny":[{"match":"github.com/*/answers.git**","except":["`+lsRemote+`"]},"evil.test"]}`)
	var fromJSON Policy
	require.NoError(t, json.Unmarshal(j, &fromJSON))

	for name, out := range map[string]Policy{"yaml": fromYAML, "json": fromJSON} {
		for i := range in.Allow {
			assert.That(t, out.Allow[i].Same(in.Allow[i]), "%s allow %d: %+v", name, i, out.Allow[i])
		}
		for i := range in.Deny {
			assert.That(t, out.Deny[i].Same(in.Deny[i]), "%s deny %d: %+v", name, i, out.Deny[i])
		}
		_, err := out.Rule()
		require.NoError(t, err, name)
	}

	// a policy file refuses what it does not know inside an entry too.
	_, err = ParsePolicy([]byte("deny:\n  - match: 'a.test/**'\n    except: [/x]\n    when: 'True'\n"))
	assert.Error(t, err, `unknown field "when"`)
	assert.That(t, !strings.Contains(string(y), "except: []"))
}
