package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// pats harness pin fetches a version for each arch and prints what its bytes
// hash to, as a clis entry and as a harness item's pin. what it fetched is
// in the cache under its sha.
func TestPinCLI(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_, _ = w.Write([]byte("bytes of " + r.URL.Path))
	}))
	defer srv.Close()
	sha := func(path string) string {
		sum := sha256.Sum256([]byte("bytes of " + path))
		return hex.EncodeToString(sum[:])
	}
	zero := strings.Repeat("0", 64)
	tb := tables(t, `
clis:
  tool:
    version: 1.0.0
    artefacts:
      `+srv.URL+`/tool-${version}-x64.tgz:
        arch: [amd64]
        sha256: `+zero+`
        bin: package/tool
      `+srv.URL+`/tool-${version}-arm64.tgz:
        arch: [arm64]
        sha256: `+zero+`
        bin: package/tool
  both:
    version: 1.0.0
    artefacts:
      `+srv.URL+`/both-${version}:
        arch: [amd64, arm64]
        sha256: `+zero+`
  half:
    version: 1.0.0
    artefacts:
      `+srv.URL+`/half-${version}:
        arch: [amd64]
        sha256: `+zero+`
`)
	var out bytes.Buffer
	require.NoError(t, PinCLI(context.Background(), tb, "tool", "2.5.0", nil, &out))
	x64, arm := sha("/tool-2.5.0-x64.tgz"), sha("/tool-2.5.0-arm64.tgz")
	assert.ContainsString(t, out.String(), "# tool 2.5.0, as its source served it just now: trust on first use.")
	assert.ContainsString(t, out.String(), `clis:
  tool:
    version: 2.5.0
    artefacts:
      `+srv.URL+`/tool-${version}-x64.tgz:
        arch: [amd64]
        sha256: `+x64+`
        bin: package/tool
      `+srv.URL+`/tool-${version}-arm64.tgz:
        arch: [arm64]
        sha256: `+arm+`
        bin: package/tool
`)
	assert.ContainsString(t, out.String(), "    version: 2.5.0\n    sha256:\n      amd64: "+x64+"\n      arm64: "+arm+"\n")
	for _, s := range []string{x64, arm} {
		_, err := os.Stat(filepath.Join(cache, "pats", "harness", s))
		require.NoError(t, err)
	}

	// one file for both arches is fetched once, and has no bin to print.
	asked = nil
	out.Reset()
	require.NoError(t, PinCLI(context.Background(), tb, "both", "3.0.0", nil, &out))
	assert.Len(t, asked, 1)
	one := sha("/both-3.0.0")
	assert.ContainsString(t, out.String(), "        arch: [amd64, arm64]\n        sha256: "+one+"\n\n")
	assert.ContainsString(t, out.String(), "      amd64: "+one+"\n      arm64: "+one+"\n")

	assert.Error(t, PinCLI(context.Background(), tb, "half", "1.0.0", nil, &out), `cli "half" has no artefact for arm64`)
	assert.Error(t, PinCLI(context.Background(), tb, "nope", "1.0.0", nil, &out), `unknown cli "nope" (both, half, tool)`)
}
