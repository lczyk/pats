package eval

import (
	"context"
	"os"
	"os/exec"
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

func TestResolveBuildSpec(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile.other"), []byte("FROM scratch\n"), 0o644))

	// a dir spec -> context is the dir, Dockerfile inside it.
	ctxDir, df, err := resolveBuildSpec(dir, ".")
	require.NoError(t, err)
	assert.Equal(t, ctxDir, dir)
	assert.Equal(t, df, filepath.Join(dir, "Dockerfile"))

	// a file spec -> context is its dir.
	ctxDir, df, err = resolveBuildSpec(dir, "Dockerfile.other")
	require.NoError(t, err)
	assert.Equal(t, ctxDir, dir)
	assert.Equal(t, df, filepath.Join(dir, "Dockerfile.other"))

	// missing spec -> error.
	_, _, err = resolveBuildSpec(dir, "nope")
	assert.Error(t, err, "build context")
}

func TestIgnoreMatcher(t *testing.T) {
	pats := parseIgnore("# comment\n\n.pats/\n*.log\nsub/**\n!sub/keep.txt\nbuild\n")
	for rel, want := range map[string]bool{
		".pats":            true, // trailing-slash dir pattern
		".pats/runs/1/x":   true, // parent-dir coverage
		"a.log":            true,
		"deep/a.log":       false, // *.log is anchored to the root, docker-style
		"sub/anything":     true,  // /** spans
		"sub/deep/er":      true,
		"sub/keep.txt":     false, // negation, last match wins
		"build":            true,
		"builder":          false, // no partial-segment match
		"Dockerfile":       false,
		"src/main.go":      false,
		".pats-cache/file": false, // .pats doesn't cover siblings
	} {
		assert.Equal(t, ignored(pats, rel), want, "rel: %q", rel)
	}
}

func TestBuildContextHash(t *testing.T) {
	mk := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			p := filepath.Join(dir, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		}
		return dir
	}
	copies := "FROM scratch\nCOPY x /x\n"

	// deterministic across calls.
	dir := mk(t, map[string]string{"Dockerfile": copies, "x": "1"})
	h1, err := buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	h2, err := buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// a content edit changes the hash.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("2"), 0o644))
	h3, err := buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	assert.That(t, h1 != h3, "content edit did not change the hash")

	// an ignored file doesn't participate: churn in it keeps the hash stable.
	dir = mk(t, map[string]string{
		"Dockerfile":     copies,
		".dockerignore":  ".pats/\n",
		"x":              "1",
		".pats/runs/log": "a",
	})
	h1, err = buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pats", "runs", "log"), []byte("bbbb"), 0o644))
	h2, err = buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// a context-less dockerfile hashes the dockerfile alone -- context churn
	// (even unignored) can't invalidate it.
	dir = mk(t, map[string]string{"Dockerfile": "FROM ubuntu\nRUN true\n", "x": "1"})
	h1, err = buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("2"), 0o644))
	h2, err = buildContextHash(dir, filepath.Join(dir, "Dockerfile"))
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
}

// e2e: a tiny no-network context builds and yields an image id; an unchanged
// context is reused via the hash label without rebuilding; an edit misses.
func TestBuildImage(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o644))

	ctxDir, df, err := resolveBuildSpec(dir, ".")
	require.NoError(t, err)
	hash, err := buildContextHash(ctxDir, df)
	require.NoError(t, err)

	// nothing labelled with this hash yet.
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash), "")

	id, err := buildImage(context.Background(), "docker", ctxDir, df, hash)
	require.NoError(t, err)
	assert.ContainsString(t, id, "sha256:")
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", id).Run() })

	// same inputs -> the label lookup finds the image, no rebuild needed.
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash), id)

	// an edit changes the hash -> lookup misses.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("changed\n"), 0o644))
	hash2, err := buildContextHash(ctxDir, df)
	require.NoError(t, err)
	assert.That(t, hash != hash2, "edit did not change the hash")
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash2), "")

	// empty hash (hashing failed upstream) never matches anything.
	assert.Equal(t, cachedImageID(context.Background(), "docker", ""), "")
}
