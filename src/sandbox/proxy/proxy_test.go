package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
		if got := c.r.permits(c.host); got != c.want {
			t.Errorf("permits(%q) = %v, want %v", c.host, got, c.want)
		}
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
	proxy := httptest.NewServer(Handler(r, nil, nil, io.Discard))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plain:/ok" {
		t.Fatalf("allowed: got %d %q", resp.StatusCode, body)
	}

	resp, err = client.Get(upstream.URL + "/secret/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied url: got %d, want 403", resp.StatusCode)
	}

	// unlisted host dies at the host gate.
	resp, err = client.Get("http://203.0.113.1/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied host: got %d, want 403", resp.StatusCode)
	}
}
