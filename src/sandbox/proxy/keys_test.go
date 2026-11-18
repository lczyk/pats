package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"gopkg.in/yaml.v3"
)

// orPrefix is split so the key below reads as a fixture, not a secret.
const orPrefix = "sk-" + "or-v1-"

var testReal = orPrefix + strings.Repeat("0123456789abcdef", 4)

const bindLine = "POST openrouter.test/api/v1/chat/completions"

// a second endpoint of the same key, which takes it bare in a header of its
// own.
const msgLine = "POST openrouter.test/api/v1/messages"

var testBind = []KeyEntry{{Match: bindLine}, {Match: msgLine, Header: "x-api-key"}}

func TestPlaceholder(t *testing.T) {
	ph, err := Placeholder(testReal, nil)
	require.NoError(t, err)
	assert.Equal(t, len(ph), len(testReal))
	assert.That(t, strings.HasPrefix(ph, orPrefix), "prefix lost: %s", ph)
	assert.That(t, strings.Trim(ph[len(orPrefix):], hexDigits) == "", "not hex: %s", ph)
	assert.That(t, ph != testReal, "placeholder is the key")
	other, err := Placeholder(testReal, nil)
	require.NoError(t, err)
	assert.That(t, other != ph, "two placeholders alike")

	// no dash: no prefix; letters and digits past it.
	ph, err = Placeholder("ABCdef0123456789XYZ", nil)
	require.NoError(t, err)
	assert.Equal(t, len(ph), 19)
	assert.That(t, strings.Trim(ph, alnum) == "", "alphabet: %s", ph)

	for _, bad := range []string{"", orPrefix + "short", orPrefix + "0123456789abcdef 0123", "sk-\n0123456789abcdef0123",
		"sk-\u00e90123456789abcdef0123", strings.Repeat("a", maxKey+1)} {
		_, err := Placeholder(bad, nil)
		assert.That(t, err != nil, "took %q", bad)
	}
}

func TestHoldKeys(t *testing.T) {
	ph, err := Placeholder(testReal, nil)
	require.NoError(t, err)
	ok := []KeyBinding{{Name: "K", Placeholder: ph, Bind: KeyLines(bindLine)}}
	_, err = holdKeys(ok, map[string]string{ph: testReal})
	require.NoError(t, err)

	for name, c := range map[string]struct {
		b    []KeyBinding
		vals map[string]string
		want string
	}{
		"no value":       {ok, map[string]string{}, "no value"},
		"bad value":      {ok, map[string]string{ph: "short"}, "past its prefix"},
		"length differs": {ok, map[string]string{ph: testReal + "0"}, "does not stand in"},
		"no method":      {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: KeyLines("openrouter.test/api/**")}}, map[string]string{ph: testReal}, "names a method"},
		"bare host":      {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: KeyLines("openrouter.test")}}, map[string]string{ph: testReal}, "names a method"},
		"suffix host":    {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: KeyLines("POST .openrouter.test/x")}}, map[string]string{ph: testReal}, "names a method"},
		"ip":             {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: KeyLines("POST 10.0.0.1/x")}}, map[string]string{ph: testReal}, "names a method"},
		"twice":          {append(ok, ok[0]), map[string]string{ph: testReal}, "used twice"},
		"no entry":       {[]KeyBinding{{Name: "K", Placeholder: ph}}, map[string]string{ph: testReal}, "needs"},
		"bad header":     {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: []KeyEntry{{Match: bindLine, Header: "x api"}}}}, map[string]string{ph: testReal}, "not a header's name"},
		"bearer header":  {[]KeyBinding{{Name: "K", Placeholder: ph, Bind: []KeyEntry{{Match: bindLine, Header: "authorization"}}}}, map[string]string{ph: testReal}, "already takes the key there"},
	} {
		_, err := holdKeys(c.b, c.vals)
		assert.Error(t, err, c.want, name)
	}
}

// keyRig is a proxy holding one key bound to openrouter.test, in front of a
// recording upstream that answers as handler says.
type keyRig struct {
	proxy    *Proxy
	ph       string
	client   *http.Client
	audit    *auditBuf
	mu       sync.Mutex
	seen     []*http.Request
	upstream *httptest.Server
}

func (k *keyRig) got() []*http.Request {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*http.Request(nil), k.seen...)
}

func newKeyRig(t *testing.T, allow, deny []string, methods map[string]bool, handler http.HandlerFunc) *keyRig {
	t.Helper()
	k := &keyRig{audit: &auditBuf{}}
	k.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		k.mu.Lock()
		k.seen = append(k.seen, r)
		k.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(k.upstream.Close)
	upAddr := k.upstream.Listener.Addr().String()
	upPool := x509.NewCertPool()
	upPool.AddCert(k.upstream.Certificate())

	certPath, keyPath, pool := testCA(t, t.TempDir())
	s, err := NewSigner(certPath, keyPath)
	require.NoError(t, err)
	k.ph, err = Placeholder(testReal, nil)
	require.NoError(t, err)
	held, err := holdKeys([]KeyBinding{{Name: "OPENROUTER_API_KEY", Placeholder: k.ph, Bind: testBind}},
		map[string]string{k.ph: testReal})
	require.NoError(t, err)
	rule := mustRule(t, false, allow, deny)
	rule.Methods = methods
	p := &Proxy{
		Rule:   rule,
		Signer: s,
		// every origin is the test server, whose certificate is for
		// example.com.
		Upstream: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upAddr)
			},
			TLSClientConfig: &tls.Config{RootCAs: upPool, ServerName: "example.com"},
		},
		Audit: k.audit,
		keys:  held,
	}
	k.proxy = p
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	proxyURL, _ := url.Parse(srv.URL)
	k.client = &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool},
			DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return k
}

func (k *keyRig) do(t *testing.T, method, rawURL string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(`{"model":"m","note":"`+k.ph+`"}`))
	require.NoError(t, err)
	for h, v := range hdr {
		req.Header.Set(h, v)
	}
	resp, err := k.client.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

const chat = "https://openrouter.test/api/v1/chat/completions"

// the placeholder in its one header, on its binding, goes out as the real
// key; the body (which may quote the placeholder) and every other header
// stay as they were.
func TestKeySubstituted(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, nil)
	resp, _ := k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph, "X-Title": "opencode"})
	assert.Equal(t, resp.StatusCode, 200)
	got := k.got()
	require.Len(t, got, 1)
	assert.Equal(t, got[0].Header.Get("Authorization"), "Bearer "+testReal)
	assert.Equal(t, got[0].Header.Get("X-Title"), "opencode")
	body, _ := io.ReadAll(got[0].Body)
	assert.ContainsString(t, string(body), k.ph)
	ev := k.audit.last(t)
	assert.Equal(t, ev.Key, "substituted")
	assert.Equal(t, ev.Allowed, true)
	assert.That(t, !strings.Contains(k.audit.buf.String(), testReal), "the real key in the audit")

	// a bound request without the header goes out as it is.
	resp, _ = k.do(t, "POST", chat, nil)
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, k.got()[1].Header.Get("Authorization"), "")
	assert.Equal(t, k.audit.last(t).Key, "")
}

// a placeholder anywhere but its binding's one header is refused, before
// the policy is asked, and never reaches the upstream.
func TestKeyMisuse(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", "other.test/**", "openrouter.test:8443/**", bindLine}, nil, nil, nil)
	bearerPH := "Bearer " + k.ph
	for name, c := range map[string]struct {
		method, url string
		hdr         map[string]string
	}{
		"another header":     {"POST", chat, map[string]string{"Authorization": bearerPH, "X-Api-Key": k.ph}},
		"in the query":       {"POST", chat + "?key=" + k.ph, nil},
		"in the path":        {"POST", "https://openrouter.test/api/v1/" + k.ph, nil},
		"another shape":      {"POST", chat, map[string]string{"Authorization": "Token " + k.ph}},
		"bare placeholder":   {"POST", chat, map[string]string{"Authorization": k.ph}},
		"other auth, bound":  {"POST", chat, map[string]string{"Authorization": "Bearer " + orPrefix + "somethingelse0123456789"}},
		"another host":       {"POST", "https://other.test/api/v1/chat/completions", map[string]string{"Authorization": bearerPH}},
		"another method":     {"GET", chat, map[string]string{"Authorization": bearerPH}},
		"another path":       {"POST", "https://openrouter.test/api/v1/models", map[string]string{"Authorization": bearerPH}},
		"another port":       {"POST", "https://openrouter.test:8443/api/v1/chat/completions", map[string]string{"Authorization": bearerPH}},
		"dot segments":       {"POST", "https://openrouter.test/api/v1/x/%2e%2e/chat/completions", map[string]string{"Authorization": bearerPH}},
		"doubled slash":      {"POST", "https://openrouter.test/api/v1//chat/completions", map[string]string{"Authorization": bearerPH}},
		"in the user agent":  {"POST", chat, map[string]string{"User-Agent": "x " + k.ph}},
		"plain http, bound":  {"POST", "http://openrouter.test/api/v1/chat/completions", map[string]string{"Authorization": bearerPH}},
		"plain http, other":  {"GET", "http://other.test/", map[string]string{"X-Leak": k.ph}},
		"escaped slash":      {"POST", "https://openrouter.test/api/v1/chat%2fcompletions", map[string]string{"Authorization": bearerPH}},
		"escaped in query":   {"POST", chat + "?k=" + strings.ReplaceAll(k.ph, "-", "%2D"), nil},
		"tls on port 80":     {"POST", "https://openrouter.test:80/api/v1/chat/completions", map[string]string{"Authorization": bearerPH}},
		"in a header's name": {"POST", chat, map[string]string{"X-" + k.ph: "1"}},
	} {
		before := len(k.got())
		resp, body := k.do(t, c.method, c.url, c.hdr)
		assert.Equal(t, resp.StatusCode, http.StatusForbidden, name)
		assert.Equal(t, body, "egress denied: key misuse\n", name) // not the policy's message: no policy was asked
		assert.That(t, !strings.Contains(body, k.ph), "%s: the refusal names the placeholder", name)
		assert.Equal(t, len(k.got()), before, name)
		ev := k.audit.last(t)
		assert.Equal(t, ev.Rule, "key", name)
		assert.Equal(t, ev.Reason, "misuse", name)
		assert.That(t, !strings.Contains(ev.URL+ev.UA, k.ph), "%s: the audit names the placeholder: %s %s", name, ev.URL, ev.UA)
	}
}

const messages = "https://openrouter.test/api/v1/messages"

// an endpoint bound to a header of its own takes the placeholder as that
// header's whole value, and nothing else: the bearer shape is the other
// endpoint's.
func TestKeyHeader(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine, msgLine}, nil, nil, nil)
	for _, name := range []string{"X-Api-Key", "x-api-key"} {
		req, err := http.NewRequest("POST", messages+"?beta=true", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header[name] = []string{k.ph}
		req.Header.Set("Anthropic-Version", "2023-06-01")
		resp, err := k.client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, resp.StatusCode, 200, name)
		got := k.got()
		last := got[len(got)-1]
		assert.EqualArrays(t, last.Header.Values("X-Api-Key"), []string{testReal})
		assert.Equal(t, last.Header.Get("Authorization"), "")
		assert.Equal(t, last.Header.Get("Anthropic-Version"), "2023-06-01")
		assert.Equal(t, last.URL.RawQuery, "beta=true")
		ev := k.audit.last(t)
		assert.Equal(t, ev.Key, "substituted", name)
		assert.Equal(t, ev.Allowed, true, name)
	}
	assert.That(t, !strings.Contains(k.audit.buf.String(), testReal), "the real key in the audit")

	// with no key header, or another credential beside the spent one, the
	// request goes out as it is but for the key.
	resp, _ := k.do(t, "POST", messages, nil)
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, k.audit.last(t).Key, "")
	resp, _ = k.do(t, "POST", messages, map[string]string{"X-Api-Key": k.ph, "Authorization": "Bearer mine"})
	assert.Equal(t, resp.StatusCode, 200)
	got := k.got()
	assert.Equal(t, got[len(got)-1].Header.Get("X-Api-Key"), testReal)
	assert.Equal(t, got[len(got)-1].Header.Get("Authorization"), "Bearer mine")

	// the answer on it is scrubbed as the other endpoint's is.
	k = newKeyRig(t, []string{"openrouter.test"}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "you sent "+r.Header.Get("X-Api-Key"))
	})
	_, body := k.do(t, "POST", messages, map[string]string{"X-Api-Key": k.ph})
	assert.Equal(t, body, "you sent "+k.ph)

	k = newKeyRig(t, []string{"openrouter.test", bindLine, msgLine}, nil, nil, nil)
	bearerPH := "Bearer " + k.ph
	for name, c := range map[string]struct {
		url string
		hdr map[string][]string
	}{
		"bearer on the header endpoint": {messages, map[string][]string{"Authorization": {bearerPH}}},
		"bearer shape in the header":    {messages, map[string][]string{"X-Api-Key": {bearerPH}}},
		"another value":                 {messages, map[string][]string{"X-Api-Key": {"sk-mine"}}},
		"longer":                        {messages, map[string][]string{"X-Api-Key": {k.ph + "0"}}},
		"twice":                         {messages, map[string][]string{"X-Api-Key": {k.ph, k.ph}}},
		"both headers":                  {messages, map[string][]string{"X-Api-Key": {k.ph}, "Authorization": {bearerPH}}},
		"the header on the bearer one":  {chat, map[string][]string{"X-Api-Key": {k.ph}}},
		"both, on the bearer one":       {chat, map[string][]string{"X-Api-Key": {k.ph}, "Authorization": {bearerPH}}},
		"a path below":                  {messages + "/count_tokens", map[string][]string{"X-Api-Key": {k.ph}}},
		"another header":                {messages, map[string][]string{"Api-Key": {k.ph}}},
	} {
		before := len(k.got())
		req, err := http.NewRequest("POST", c.url, strings.NewReader("{}"))
		require.NoError(t, err)
		for h, v := range c.hdr {
			req.Header[h] = v
		}
		resp, err := k.client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusForbidden, name)
		assert.Equal(t, len(k.got()), before, name)
		ev := k.audit.last(t)
		assert.Equal(t, ev.Rule, "key", name)
		assert.Equal(t, ev.Reason, "misuse", name)
	}
}

// an endpoint is a line, or a mapping that names its header.
func TestKeyEntryForms(t *testing.T) {
	want := []KeyEntry{{Match: bindLine}, {Match: msgLine, Header: "X-Api-Key"}}
	y := "- " + bindLine + "\n- {match: " + msgLine + ", header: X-Api-Key}\n"
	var got []KeyEntry
	require.NoError(t, yaml.Unmarshal([]byte(y), &got))
	assert.EqualArrays(t, got, want)
	out, err := yaml.Marshal(want)
	require.NoError(t, err)
	got = nil
	require.NoError(t, yaml.Unmarshal(out, &got))
	assert.EqualArrays(t, got, want)

	j, err := json.Marshal(want)
	require.NoError(t, err)
	assert.Equal(t, string(j), `["`+bindLine+`",{"match":"`+msgLine+`","header":"X-Api-Key"}]`)
	got = nil
	require.NoError(t, json.Unmarshal(j, &got))
	assert.EqualArrays(t, got, want)
	assert.Equal(t, want[0].String(), bindLine+" (Authorization: Bearer)")
	assert.Equal(t, KeyEntry{Match: msgLine, Header: "x-api-key"}.String(), msgLine+" (X-Api-Key)")
	assert.EqualArrays(t, KeyMatches(want), []string{bindLine, msgLine})

	for src, why := range map[string]string{
		"- {match: POST a.test/x}":                       "both set",
		"- {header: X-Api-Key}":                          "both set",
		"- {match: POST a.test/x, header: X, prefix: y}": "unknown field",
		"- {match: POST a.test/x, header: X, header: Y}": "written twice",
		"- {match: POST a.test/x, header: [X]}":          "want a line",
		"- [POST a.test/x]":                              "a line, or a mapping",
	} {
		var e []KeyEntry
		assert.Error(t, yaml.Unmarshal([]byte(src), &e), why, src)
	}
	for src, why := range map[string]string{
		`[{"match":"POST a.test/x"}]`:                           "both set",
		`[{"match":"POST a.test/x","header":"X","Header":"Y"}]`: "unknown field",
		`[{"match":"POST a.test/x","header":"X","header":"Y"}]`: "written twice",
		`[{"match":"POST a.test/x","header":7}]`:                "header",
		`[7]`:                                                   "a line, or a mapping",
	} {
		var e []KeyEntry
		assert.Error(t, json.Unmarshal([]byte(src), &e), why, src)
	}
}

// the policy still decides a bound request: an explicit deny wins, and a
// bound method passes a methods posture through its entry's own method.
func TestKeyPolicy(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, []string{"openrouter.test/api/v1/chat/**"}, nil, nil)
	resp, _ := k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Equal(t, k.audit.last(t).Rule, "openrouter.test/api/v1/chat/**")
	assert.Len(t, k.got(), 0)

	k = newKeyRig(t, []string{"openrouter.test", bindLine}, nil, map[string]bool{"GET": true, "HEAD": true}, nil)
	resp, _ = k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, k.got()[0].Header.Get("Authorization"), "Bearer "+testReal)

	// a bound host is terminated with only a bare allow entry for it.
	k = newKeyRig(t, []string{"openrouter.test"}, nil, nil, nil)
	resp, _ = k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, k.got()[0].Header.Get("Authorization"), "Bearer "+testReal)
}

// a redirect goes back to the client: the proxy follows none, and the
// client's next request to another host is refused.
func TestKeyRedirect(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", "other.test/**", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.test/steal", http.StatusFound)
	})
	resp, _ := k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, http.StatusFound)
	assert.Len(t, k.got(), 1)
	resp, _ = k.do(t, "POST", "https://other.test/steal", map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Len(t, k.got(), 1)
}

// an answer echoing the real key reaches the client with the placeholder:
// in a header, a trailer and the body, gzip'd or not, whatever the status.
func TestKeyScrub(t *testing.T) {
	for _, status := range []int{200, 206, 401} {
		k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Echo", "key="+testReal)
			w.Header().Set("Trailer", "X-Late")
			body := "you sent " + testReal + " twice: " + testReal
			if r.Header.Get("Accept-Encoding") == "gzip" {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(status)
				gz := gzip.NewWriter(w)
				io.WriteString(gz, body)
				gz.Close()
			} else {
				w.WriteHeader(status)
				io.WriteString(w, body)
			}
			w.Header().Set("X-Late", testReal)
		})
		req, _ := http.NewRequest("POST", chat, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+k.ph)
		req.Header.Set("Accept-Encoding", "br, zstd")
		resp, err := k.client.Do(req)
		require.NoError(t, err)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, resp.StatusCode, status)
		// the client's encodings were dropped: go's transport asked for gzip.
		assert.Equal(t, k.got()[0].Header.Get("Accept-Encoding"), "gzip")
		for where, s := range map[string]string{"header": resp.Header.Get("X-Echo"), "body": string(b), "trailer": resp.Trailer.Get("X-Late")} {
			assert.That(t, !strings.Contains(s, testReal), "%d %s: %q", status, where, s)
			assert.ContainsString(t, s, k.ph, status, where)
		}
	}
}

// an answer still encoded after the transport cannot be scrubbed: refused.
func TestKeyScrubEncoded(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		io.WriteString(w, "opaque")
	})
	resp, _ := k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	assert.Equal(t, k.audit.last(t).Reason, "key-scrub")
}

// a streamed answer flows event by event: a short event followed by an
// idle upstream arrives at once.
func TestKeyScrubStreams(t *testing.T) {
	release := make(chan struct{})
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hi\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: [DONE]\n\n")
	})
	defer close(release)
	req, _ := http.NewRequest("POST", chat, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+k.ph)
	resp, err := k.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		assert.Equal(t, s, "data: hi\n\n")
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was held back")
	}
}

// the body scrub across reads: a key split anywhere is caught, a held tail
// at the end goes out as the placeholder's beginning, and on an error it is
// dropped.
func TestScrubBody(t *testing.T) {
	k := &heldKey{placeholder: strings.Repeat("P", len(testReal)), real: testReal}
	in := "a" + testReal + "b" + testReal + "c" + testReal[:12]
	for n := 1; n <= len(in); n += 7 {
		r := iotest.OneByteReader(strings.NewReader(in))
		if n > 1 {
			r = iotest.HalfReader(strings.NewReader(in))
		}
		out, err := io.ReadAll(newScrubBody(io.NopCloser(r), k, nil))
		require.NoError(t, err)
		want := "a" + k.placeholder + "b" + k.placeholder + "c" + k.placeholder[:12]
		assert.Equal(t, string(out), want)
	}
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("x"+testReal[:20]), iotest.ErrReader(boom))
	out, err := io.ReadAll(newScrubBody(io.NopCloser(r), k, nil))
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, string(out), "x")
}

// the policy carries the bindings by known fields; the real values come
// apart from it.
func TestPolicyKeysRoundTrip(t *testing.T) {
	p, err := ParsePolicy([]byte("allow: [openrouter.test]\nkeys:\n  - name: K\n    placeholder: ph\n    bind: [\"" + bindLine + "\"]\n"))
	require.NoError(t, err)
	assert.Equal(t, p.Keys[0].Bind[0], KeyEntry{Match: bindLine})
	p, err = ParsePolicy([]byte("allow: [openrouter.test]\nkeys:\n  - name: K\n    placeholder: ph\n    bind: [{match: \"" + msgLine + "\", header: X-Api-Key}]\n"))
	require.NoError(t, err)
	assert.Equal(t, p.Keys[0].Bind[0], KeyEntry{Match: msgLine, Header: "X-Api-Key"})
	_, err = ParsePolicy([]byte("keys:\n  - name: K\n    placeholder: ph\n    bind: [{match: \"" + msgLine + "\", heder: X-Api-Key}]\n"))
	assert.Error(t, err, "unknown field")
	assert.That(t, p.NeedsCA(), "keys need the CA")
	_, err = ParsePolicy([]byte("keyz: []\n"))
	assert.Error(t, err, "keyz")
	m, err := ParseKeys(`{"ph":"real"}`)
	require.NoError(t, err)
	assert.Equal(t, m["ph"], "real")
	_, err = ParseKeys(`["x"]`)
	assert.Error(t, err, "json object")
}

// the request's own Host must name the port the tunnel was opened to.
func TestKeyHostPort(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, nil)
	req, _ := http.NewRequest("POST", chat, strings.NewReader("{}"))
	req.Host = "openrouter.test:8443"
	req.Header.Set("Authorization", "Bearer "+k.ph)
	resp, err := k.client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
	assert.Len(t, k.got(), 0)
}

// two keys bound on one endpoint: each placeholder spends its own key.
func TestKeysShareAnEndpoint(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, nil)
	second := orPrefix + strings.Repeat("fedcba9876543210", 4)
	ph2, err := Placeholder(second, nil)
	require.NoError(t, err)
	more, err := holdKeys([]KeyBinding{{Name: "B", Placeholder: ph2, Bind: KeyLines(bindLine)}}, map[string]string{ph2: second})
	require.NoError(t, err)
	k.proxy.keys = append(k.proxy.keys, more...)
	for ph, real := range map[string]string{k.ph: testReal, ph2: second} {
		resp, _ := k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + ph})
		assert.Equal(t, resp.StatusCode, 200)
		got := k.got()
		assert.Equal(t, got[len(got)-1].Header.Get("Authorization"), "Bearer "+real)
	}
}

// a bound endpoint's answer is scrubbed whether or not the request spent
// the key, header names included.
func TestKeyScrubUnspentAndNames(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["X-"+testReal] = []string{"1"}
		io.WriteString(w, testReal)
	})
	resp, body := k.do(t, "POST", chat, nil)
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, body, k.ph)
	for name := range resp.Header {
		assert.That(t, !strings.Contains(strings.ToLower(name), strings.ToLower(testReal)), "header name %s", name)
	}
}

// a request's trailers come too late to check: where keys are held, they
// do not go on.
func TestKeyRequestTrailersDropped(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		var names []string
		for n := range r.Trailer {
			names = append(names, n)
		}
		w.Header().Set("X-Trailers", strings.Join(names, ","))
	})
	tb := &trailerBody{r: strings.NewReader("{}")}
	req, _ := http.NewRequest("POST", chat, tb)
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Leak": nil}
	tb.set = func() { req.Trailer.Set("X-Leak", k.ph) }
	resp, err := k.client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, resp.Header.Get("X-Trailers"), "")
}

type trailerBody struct {
	r   io.Reader
	set func()
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF && b.set != nil {
		b.set()
	}
	return n, err
}

func (b *trailerBody) Close() error { return nil }

// a placeholder written over the real value can spell it anew with what
// follows: written over again, until none is left.
func TestScrubSeam(t *testing.T) {
	k := &heldKey{real: "0123456789abcdef", placeholder: "fedcba9876543210"}
	in := k.real + "123456789abcdef"
	assert.That(t, !strings.Contains(scrubbed(in, k), k.real), "scrubbed: %s", scrubbed(in, k))
	out, err := io.ReadAll(newScrubBody(io.NopCloser(iotest.OneByteReader(strings.NewReader(in))), k, nil))
	require.NoError(t, err)
	assert.That(t, !strings.Contains(string(out), k.real), "streamed: %s", out)
}

// an escaped query leaves a plain path bound; a request whose upstream
// never answered spent no key, and its line says none went out.
func TestKeyQueryAndFailedUpstream(t *testing.T) {
	k := newKeyRig(t, []string{"openrouter.test", bindLine}, nil, nil, nil)
	resp, _ := k.do(t, "POST", chat+"?x=a%20b", map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, k.got()[0].Header.Get("Authorization"), "Bearer "+testReal)

	k.upstream.Close()
	resp, _ = k.do(t, "POST", chat, map[string]string{"Authorization": "Bearer " + k.ph})
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	assert.Equal(t, k.audit.last(t).Key, "")
}
