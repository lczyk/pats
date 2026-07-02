package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestIgnoresPats(t *testing.T) {
	for _, ok := range []string{".pats", ".pats/", "/.pats", "./.pats", ".pats/**", "node_modules\n.pats/\n*.log"} {
		if !ignoresPats(ok) {
			t.Errorf("ignoresPats(%q) = false, want true", ok)
		}
	}
	// COVER: a glob that happens to cover .pats is deliberately not recognised.
	for _, bad := range []string{"", "pats", ".pats2", ".*", "**/.pats-cache"} {
		if ignoresPats(bad) {
			t.Errorf("ignoresPats(%q) = true, want false", bad)
		}
	}
}

func TestUsesContext(t *testing.T) {
	mk := func(body string) string {
		p := filepath.Join(t.TempDir(), "Dockerfile")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		return p
	}
	for body, want := range map[string]bool{
		"FROM scratch\nCOPY x /x\n":                         true,
		"FROM scratch\n  add x /x\n":                        true, // indented + lowercase
		"FROM a AS b\nFROM c\nCOPY --from=b /x /x\n":        false,
		"FROM ubuntu\nRUN apt-get update\n":                 false,
		"# COPY mentioned in a comment only\nFROM ubuntu\n": false,
	} {
		assert.Equal(t, usesContext(mk(body)), want, "dockerfile: %q", body)
	}
	// unreadable dockerfile -> assume it uses the context (fail-safe).
	assert.Equal(t, usesContext(filepath.Join(t.TempDir(), "nope")), true)
}

func TestCheckPatsIgnored(t *testing.T) {
	copies := "FROM scratch\nCOPY x /x\n"
	mk := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".pats"), 0o755))
		for name, body := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
		}
		return dir
	}

	// no .pats in the context -> fine regardless of ignores.
	assert.NoError(t, checkPatsIgnored(t.TempDir(), "Dockerfile"))

	// .pats present, dockerfile COPYs, no ignore file -> refused.
	dir := mk(t, map[string]string{"Dockerfile": copies})
	assert.Error(t, checkPatsIgnored(dir, filepath.Join(dir, "Dockerfile")), ".dockerignore")

	// .pats present but the dockerfile never reads the context -> fine.
	dir = mk(t, map[string]string{"Dockerfile": "FROM ubuntu\nRUN true\n"})
	assert.NoError(t, checkPatsIgnored(dir, filepath.Join(dir, "Dockerfile")))

	// .dockerignore excluding .pats -> fine.
	dir = mk(t, map[string]string{"Dockerfile": copies, ".dockerignore": "foo\n.pats/\n"})
	assert.NoError(t, checkPatsIgnored(dir, filepath.Join(dir, "Dockerfile")))

	// COVER: buildkit precedence -- a per-dockerfile ignore fully replaces
	// .dockerignore, so .pats must be excluded there, not in .dockerignore.
	dir = mk(t, map[string]string{
		"Dockerfile":              copies,
		".dockerignore":           ".pats/\n",
		"Dockerfile.dockerignore": "*.log\n",
	})
	assert.Error(t, checkPatsIgnored(dir, filepath.Join(dir, "Dockerfile")), "Dockerfile.dockerignore")
}

// e2e: a tiny no-network context builds and yields an image id.
func TestBuildImage(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o644))

	id, err := buildImage(context.Background(), "docker", dir, ".")
	require.NoError(t, err)
	assert.ContainsString(t, id, "sha256:")
}
