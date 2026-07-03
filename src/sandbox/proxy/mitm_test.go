package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPermitsURL(t *testing.T) {
	r := Rule{DenyURLs: ParseURLRules([]string{
		"github.com/*/chisel-releases*",
		"raw.githubusercontent.com/canonical/chisel-releases*",
	})}

	cases := []struct {
		hostPath string
		want     bool
	}{
		{"github.com/canonical/chisel-releases", false},
		{"github.com/canonical/chisel-releases/tree/main", false},
		{"github.com/anyone/chisel-releases.git/info/refs", false}, // * crosses nothing needed; suffix glob
		{"github.com/canonical/chisel", true},
		{"github.com/canonical/other-repo", true},
		{"raw.githubusercontent.com/canonical/chisel-releases/main/x.yaml", false},
		{"raw.githubusercontent.com/other/chisel-releases/main/x.yaml", true}, // host+org anchored
		{"example.com/chisel-releases", true},                                 // host not ruled
	}
	for _, c := range cases {
		if got := r.permitsURL(c.hostPath); got != c.want {
			t.Errorf("permitsURL(%q) = %v, want %v", c.hostPath, got, c.want)
		}
	}

	// mitm set derives from rule hosts, literally.
	if !r.mitmHost("github.com") || r.mitmHost("example.com") {
		t.Error("mitmHost: want github.com mitm'd, example.com not")
	}
}

func TestPermitsURLAllowRules(t *testing.T) {
	r := Rule{
		DenyURLs:  ParseURLRules([]string{"docs.example.com/internal*"}),
		AllowURLs: ParseURLRules([]string{"docs.example.com/public*", "docs.example.com/api*"}),
	}

	cases := []struct {
		hostPath string
		want     bool
	}{
		{"docs.example.com/public/guide", true},     // allow match
		{"docs.example.com/api/v1", true},           // second allow rule
		{"docs.example.com/private/x", false},       // restricted host, no allow match
		{"docs.example.com/internal/public", false}, // deny wins even under /internal*... (deny match)
		{"other.example.com/private/x", true},       // host w/out url rules: unaffected
	}
	for _, c := range cases {
		if got := r.permitsURL(c.hostPath); got != c.want {
			t.Errorf("permitsURL(%q) = %v, want %v", c.hostPath, got, c.want)
		}
	}

	// allow-url hosts are mitm'd too.
	if !r.mitmHost("docs.example.com") {
		t.Error("mitmHost: allow-urls host must be mitm'd")
	}
}

func TestParseURLRulesRejectsWildcardHost(t *testing.T) {
	// belt-and-braces vs hand-set env: wildcard/empty host parts are dropped.
	if got := ParseURLRules([]string{"*/chisel*", "/x", "*.github.com/x"}); len(got) != 0 {
		t.Fatalf("want all rejected, got %d rules", len(got))
	}
}

// testCA mints a throwaway CA and writes it to dir, returning paths + cert pool.
func testCA(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPath = filepath.Join(dir, "ca.pem")
	keyPath = filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	pool = x509.NewCertPool()
	cert, _ := x509.ParseCertificate(der)
	pool.AddCert(cert)
	return certPath, keyPath, pool
}

// COVER: end-to-end mitm path -- CONNECT terminates tls with a CA-signed leaf,
// allowed urls reach the upstream, denied urls die with 403 before leaving.
// inline pem (the run phase passes the CA over env, not a mount -- the
// rootless proxy has no fs access) must load the same as file paths.
func TestNewSignerInlinePEM(t *testing.T) {
	certPath, keyPath, _ := testCA(t, t.TempDir())
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(string(certPEM), string(keyPEM))
	if err != nil {
		t.Fatalf("inline pem: %v", err)
	}
	if _, err := s.leaf("example.com"); err != nil {
		t.Fatalf("leaf: %v", err)
	}
}

func TestMitmE2E(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello:"+r.URL.Path)
	}))
	defer upstream.Close()

	certPath, keyPath, pool := testCA(t, t.TempDir())
	s, err := NewSigner(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	r := Rule{
		Allow:    []string{"127.0.0.1"},
		DenyURLs: ParseURLRules([]string{"127.0.0.1/secret*"}),
	}

	// the proxy's upstream transport trusts the test server's self-signed cert.
	proxy := httptest.NewServer(Handler(r, s, upstream.Client().Transport))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool}, // trusts only the mitm CA
	}}

	resp, err := client.Get(upstream.URL + "/ok")
	if err != nil {
		t.Fatalf("allowed url: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello:/ok" {
		t.Fatalf("allowed url: got %d %q", resp.StatusCode, body)
	}

	resp, err = client.Get(upstream.URL + "/secret/answer.yaml")
	if err != nil {
		t.Fatalf("denied url: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied url: got %d, want 403", resp.StatusCode)
	}

	// non-mitm'd host on the same rule set stays a plain tunnel and is refused
	// by the host gate (not on the allowlist).
	resp, err = client.Get("https://203.0.113.1/x")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected host-gate refusal for unlisted host")
	}
}

// FuzzPermitsURL: hostPath comes straight from the client's request path, so
// it's attacker-controlled. just wants no panic across rule shapes.
func FuzzPermitsURL(f *testing.F) {
	r := Rule{
		DenyURLs:  ParseURLRules([]string{"github.com/*/chisel-releases*"}),
		AllowURLs: ParseURLRules([]string{"docs.example.com/public*"}),
	}
	seeds := []string{
		"github.com/canonical/chisel-releases/tree/main",
		"docs.example.com/public/guide",
		"",
		"*/*",
		"github.com/../../etc/passwd",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, hostPath string) {
		r.permitsURL(hostPath)
	})
}

// FuzzParseURLRules: Rule patterns come from pats.yaml, but keep this cheap
// too since a hand-set env var can also feed it.
func FuzzParseURLRules(f *testing.F) {
	seeds := []string{
		"github.com/*/chisel-releases*",
		"*/x",
		"",
		"a/b/c*d*e",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, pat string) {
		ParseURLRules([]string{pat})
	})
}

// BenchmarkPermitsURL pins the per-request filtering overhead at a
// deliberately heavy rule count (50 rules; real configs have a handful).
func BenchmarkPermitsURL(b *testing.B) {
	var deny, allow []string
	for i := range 25 {
		deny = append(deny, fmt.Sprintf("host%d.example.com/*/secrets*", i))
		allow = append(allow, fmt.Sprintf("docs%d.example.com/public*", i))
	}
	r := Rule{DenyURLs: ParseURLRules(deny), AllowURLs: ParseURLRules(allow)}
	paths := []string{
		"host24.example.com/x/secrets/key.pem", // deny hit, last rule
		"docs24.example.com/public/guide",      // allow hit, last rule
		"other.example.com/anything",           // no rules for host
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		r.permitsURL(paths[i%len(paths)])
	}
}
