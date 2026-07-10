package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestPermits(t *testing.T) {
	deny := Rule{DefaultAllow: false, Allow: []string{"api.anthropic.com", ".ubuntu.com", "*.npmjs.org"}}
	allow := Rule{DefaultAllow: true, Deny: []string{"github.com", ".githubusercontent.com"}}

	cases := []struct {
		r    Rule
		host string
		want bool
	}{
		{deny, "api.anthropic.com", true},           // exact allow
		{deny, "archive.ubuntu.com", true},          // suffix allow
		{deny, "ubuntu.com", true},                  // bare suffix root
		{deny, "registry.npmjs.org", true},          // *.suffix
		{deny, "github.com", false},                 // not in allowlist
		{deny, "evil.com", false},                   // default deny
		{allow, "example.com", true},                // default allow
		{allow, "github.com", false},                // exact deny
		{allow, "raw.githubusercontent.com", false}, // suffix deny
	}
	for _, c := range cases {
		assert.Equal(t, c.r.permits(c.host), c.want)
	}
}

// COVER: the plain-http proxy path (absolute-URI requests, e.g. apt) -- host
// gate, url rules applied directly (no mitm needed), and forwarding.
func TestHandleHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain:"+r.URL.Path)
	}))
	defer upstream.Close()

	r := Rule{
		Allow:    []string{"127.0.0.1"},
		DenyURLs: ParseURLRules([]string{"127.0.0.1/secret*"}),
	}
	proxy := httptest.NewServer(Handler(r, nil, nil))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL + "/ok")
	if err != nil {
		require.NoError(t, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, 200)
	assert.Equal(t, string(body), "plain:/ok")

	resp, err = client.Get(upstream.URL + "/secret/x")
	if err != nil {
		require.NoError(t, err)
	}
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)

	// unlisted host dies at the host gate.
	resp, err = client.Get("http://203.0.113.1/x")
	if err != nil {
		require.NoError(t, err)
	}
	resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusForbidden)
}
