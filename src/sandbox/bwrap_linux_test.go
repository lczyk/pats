//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// TestMain lets this test binary stand in for the pats binary as the netns
// helper: bwrap proxy-mode runs re-exec os.Executable() with __sbx-net, which
// in tests is the test binary itself.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__sbx-net" {
		os.Exit(NetnsMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// dumpOnFail prints the sandbox's stderr when the test fails -- bwrap's own
// errors (namespace perms, mount failures) land there and are invisible
// otherwise.
func dumpOnFail(t *testing.T, out, errb *bytes.Buffer) {
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stdout: %q\nstderr: %q", out.String(), errb.String())
		}
	})
}

func bwrapOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not on PATH")
	}
}

func curlOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
}

func TestBwrapRunBasic(t *testing.T) {
	bwrapOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hi"), 0o644))

	sb, err := New("bwrap", "")
	require.NoError(t, err)

	var out, errb bytes.Buffer
	dumpOnFail(t, &out, &errb)
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "echo $PATS_GREETING; cat marker.txt"},
		Workdir: dir,
		Env:     map[string]string{"PATS_GREETING": "hello-pats", "PATH": os.Getenv("PATH")},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "hello-pats")
	assert.ContainsString(t, out.String(), "hi")
}

func TestBwrapExitCode(t *testing.T) {
	bwrapOrSkip(t)
	sb, err := New("bwrap", "")
	require.NoError(t, err)
	var out, errb bytes.Buffer
	dumpOnFail(t, &out, &errb)
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "exit 7"},
		Workdir: t.TempDir(),
		Env:     map[string]string{"PATH": os.Getenv("PATH")},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 7)
}

func TestBwrapEgressNone(t *testing.T) {
	bwrapOrSkip(t)
	curlOrSkip(t)
	// a host server proves the target is reachable outside the sandbox.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	sb, err := New("bwrap", "")
	require.NoError(t, err)
	var out, errb bytes.Buffer
	dumpOnFail(t, &out, &errb)
	code, err := sb.Run(context.Background(), Spec{
		Argv:    []string{"sh", "-c", "curl -s --max-time 2 " + upstream.URL + " || echo no-net"},
		Workdir: t.TempDir(),
		Env:     map[string]string{"PATH": os.Getenv("PATH")},
		Egress:  Egress{Mode: "none"},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "no-net")
}

// TestBwrapEgressProxy drives the whole bridge: agent netns -> in-netns
// forwarder -> unix socket -> in-process proxy -> a host http server. the
// allowed host round-trips; the denied one gets the proxy's 403; a direct
// (proxy-env-bypassing) dial has no route at all.
func TestBwrapEgressProxy(t *testing.T) {
	bwrapOrSkip(t)
	curlOrSkip(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	sb, err := New("bwrap", "")
	require.NoError(t, err)
	audit := filepath.Join(t.TempDir(), "audit.jsonl")

	var out, errb bytes.Buffer
	dumpOnFail(t, &out, &errb)
	code, err := sb.Run(context.Background(), Spec{
		Argv: []string{"sh", "-c",
			// 1: allowed via proxy; 2: denied via proxy (403); 3: bypass attempt
			// w/out the proxy -- must fail (no route), not reach upstream.
			"curl -s " + upstream.URL + "; " +
				"curl -s -o /dev/null -w '%{http_code}' -x $HTTP_PROXY http://denied.example.com; " +
				"curl -s --noproxy '*' --max-time 2 " + upstream.URL + " || echo direct-blocked"},
		Workdir: t.TempDir(),
		Env:     map[string]string{"PATH": os.Getenv("PATH")},
		Egress:  Egress{Mode: "proxy", Allow: []string{"127.0.0.1"}, AuditPath: audit},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "upstream-ok")
	assert.ContainsString(t, out.String(), "403")
	assert.ContainsString(t, out.String(), "direct-blocked")

	log, err := os.ReadFile(audit)
	require.NoError(t, err)
	assert.ContainsString(t, string(log), `"host":"127.0.0.1"`)
	assert.ContainsString(t, string(log), `"host":"denied.example.com"`)
}

// TestBwrapEgressMitm exercises url filtering end to end: tls to the host
// server is terminated by the proxy with the per-run CA (curl trusts it via
// the staged bundle), the allowed path passes, the denied path 403s.
func TestBwrapEgressMitm(t *testing.T) {
	bwrapOrSkip(t)
	curlOrSkip(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tls-ok:" + r.URL.Path))
	}))
	defer upstream.Close()
	// the proxy re-dials the real host itself, so its transport must trust the
	// httptest cert. swap the default transport for the test.
	orig := http.DefaultTransport
	http.DefaultTransport = upstream.Client().Transport
	defer func() { http.DefaultTransport = orig }()

	sb, err := New("bwrap", "")
	require.NoError(t, err)

	var out, errb bytes.Buffer
	dumpOnFail(t, &out, &errb)
	code, err := sb.Run(context.Background(), Spec{
		Argv: []string{"sh", "-c",
			"curl -s " + upstream.URL + "/open/thing; " +
				"curl -s -o /dev/null -w '%{http_code}' " + upstream.URL + "/secret/thing"},
		Workdir: t.TempDir(),
		Env:     map[string]string{"PATH": os.Getenv("PATH")},
		Egress: Egress{
			Mode:     "mitm-proxy",
			Allow:    []string{"127.0.0.1"},
			DenyURLs: []string{"127.0.0.1/secret*"},
		},
	}, &out, &errb)
	require.NoError(t, err)
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out.String(), "tls-ok:/open/thing")
	assert.ContainsString(t, out.String(), "403")
}
