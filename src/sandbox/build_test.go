package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	for body, want := range map[string]bool{
		"FROM scratch\nCOPY x /x\n":                         true,
		"FROM scratch\n  add x /x\n":                        true, // indented + lowercase
		"FROM a AS b\nFROM c\nCOPY --from=b /x /x\n":        false,
		"FROM ubuntu\nRUN apt-get update\n":                 false,
		"# COPY mentioned in a comment only\nFROM ubuntu\n": false,
	} {
		assert.Equal(t, usesContext([]byte(body), nil), want, "dockerfile: %q", body)
	}
	// unreadable dockerfile (caller's read failed) -> assume it uses the
	// context (fail-safe) -- the fail-safe now lives in usesContext itself, not
	// in a read it performs, since the caller reads the dockerfile once and
	// shares the result.
	assert.Equal(t, usesContext(nil, os.ErrNotExist), true)
}

// readDockerfile mimics what buildImages does: read the dockerfile once and
// hand the result (data, err) to the functions that used to each read it
// themselves.
func readDockerfile(dockerfile string) ([]byte, error) {
	return os.ReadFile(dockerfile)
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
	check := func(ctxDir, dockerfile string) error {
		data, err := readDockerfile(dockerfile)
		return checkPatsIgnored(ctxDir, dockerfile, data, err)
	}

	// no .pats in the context -> fine regardless of ignores.
	assert.NoError(t, check(t.TempDir(), "Dockerfile"))

	// .pats present, dockerfile COPYs, no ignore file -> refused.
	dir := mk(t, map[string]string{"Dockerfile": copies})
	assert.Error(t, check(dir, filepath.Join(dir, "Dockerfile")), ".dockerignore")

	// .pats present but the dockerfile never reads the context -> fine.
	dir = mk(t, map[string]string{"Dockerfile": "FROM ubuntu\nRUN true\n"})
	assert.NoError(t, check(dir, filepath.Join(dir, "Dockerfile")))

	// .dockerignore excluding .pats -> fine.
	dir = mk(t, map[string]string{"Dockerfile": copies, ".dockerignore": "foo\n.pats/\n"})
	assert.NoError(t, check(dir, filepath.Join(dir, "Dockerfile")))

	// COVER: buildkit precedence -- a per-dockerfile ignore fully replaces
	// .dockerignore, so .pats must be excluded there, not in .dockerignore.
	dir = mk(t, map[string]string{
		"Dockerfile":              copies,
		".dockerignore":           ".pats/\n",
		"Dockerfile.dockerignore": "*.log\n",
	})
	assert.Error(t, check(dir, filepath.Join(dir, "Dockerfile")), "Dockerfile.dockerignore")
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
	hashWith := func(dir, base string) (string, error) {
		dockerfile := filepath.Join(dir, "Dockerfile")
		data, err := readDockerfile(dockerfile)
		return buildContextHash(dir, dockerfile, data, err, base)
	}
	hashOf := func(dir string) (string, error) { return hashWith(dir, "pats/sandbox:v0") }

	// deterministic across calls.
	dir := mk(t, map[string]string{"Dockerfile": copies, "x": "1"})
	h1, err := hashOf(dir)
	require.NoError(t, err)
	h2, err := hashOf(dir)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// a content edit changes the hash.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("2"), 0o644))
	h3, err := hashOf(dir)
	require.NoError(t, err)
	assert.That(t, h1 != h3, "content edit did not change the hash")

	// an ignored file doesn't participate: churn in it keeps the hash stable.
	dir = mk(t, map[string]string{
		"Dockerfile":     copies,
		".dockerignore":  ".pats/\n",
		"x":              "1",
		".pats/runs/log": "a",
	})
	h1, err = hashOf(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pats", "runs", "log"), []byte("bbbb"), 0o644))
	h2, err = hashOf(dir)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// a context-less dockerfile hashes the dockerfile alone -- context churn
	// (even unignored) can't invalidate it.
	dir = mk(t, map[string]string{"Dockerfile": "FROM ubuntu\nRUN true\n", "x": "1"})
	h1, err = hashOf(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("2"), 0o644))
	h2, err = hashOf(dir)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// the PATS_SANDBOX base joins the hash only for a dockerfile that uses it.
	h3, err = hashWith(dir, "pats/sandbox:v1")
	require.NoError(t, err)
	assert.Equal(t, h1, h3)
	dir = mk(t, map[string]string{"Dockerfile": "ARG PATS_SANDBOX\nFROM ${PATS_SANDBOX}\nRUN true\n"})
	h1, err = hashOf(dir)
	require.NoError(t, err)
	h3, err = hashWith(dir, "pats/sandbox:v1")
	require.NoError(t, err)
	assert.That(t, h1 != h3, "base change did not change the hash of a dockerfile that uses it")
}

// e2e: a tiny no-network context builds and yields an image id; an unchanged
// context is reused via the hash label without rebuilding; an edit misses.
func TestBuildImage(t *testing.T) {
	dockerOrSkip(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o644))

	ctxDir, df := dir, filepath.Join(dir, "Dockerfile")
	dfData, dfErr := readDockerfile(df)
	require.NoError(t, dfErr)
	hash, err := buildContextHash(ctxDir, df, dfData, dfErr, "")
	require.NoError(t, err)

	// nothing labelled with this hash yet.
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash), "")

	id, err := buildImage(context.Background(), "docker", ctxDir, df, hash, "")
	require.NoError(t, err)
	assert.ContainsString(t, id, "sha256:")
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", id).Run() })

	// same inputs -> the label lookup finds the image, no rebuild needed.
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash), id)

	// an edit changes the hash -> lookup misses.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("changed\n"), 0o644))
	dfData2, dfErr2 := readDockerfile(df)
	require.NoError(t, dfErr2)
	hash2, err := buildContextHash(ctxDir, df, dfData2, dfErr2, "")
	require.NoError(t, err)
	assert.That(t, hash != hash2, "edit did not change the hash")
	assert.Equal(t, cachedImageID(context.Background(), "docker", hash2), "")

	// empty hash (hashing failed upstream) never matches anything.
	assert.Equal(t, cachedImageID(context.Background(), "docker", ""), "")
}

// the bases a dockerfile builds on, as the build resolves them: the pats arg
// substituted, stages, scratch and other args left out.
func TestFromRefs(t *testing.T) {
	df := []byte(`ARG PATS_SANDBOX
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
FROM ${PATS_SANDBOX}
from ubuntu:26.04 as tools
FROM build
FROM tools
FROM scratch
FROM $OTHER
`)
	assert.Equal(t, strings.Join(fromRefs(df, "ghcr.io/x/sandbox:v1"), " "), "golang:1.26 ghcr.io/x/sandbox:v1 ubuntu:26.04")
}

// a base refreshed locally changes the key; an absent one is pulled first, as
// the build would, so the key does not move again on the next run.
func TestBaseImageIDs(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "pulled")
	drv := filepath.Join(dir, "drv")
	script := `#!/bin/sh
case "$1" in
  image) [ -f ` + state + ` ] && echo sha256:new || { [ "$5" = present:1 ] && echo sha256:old || exit 1; } ;;
  pull) touch ` + state + ` ;;
esac
`
	require.NoError(t, os.WriteFile(drv, []byte(script), 0o755))
	ids, err := baseImageIDs(context.Background(), drv, []string{"present:1"})
	require.NoError(t, err)
	assert.Equal(t, ids, "present:1=sha256:old\n")
	ids, err = baseImageIDs(context.Background(), drv, []string{"absent:1"})
	require.NoError(t, err)
	assert.Equal(t, ids, "absent:1=sha256:new\n")
	assert.NotEqual(t, rehash("h", "a=1\n"), rehash("h", "a=2\n"))
	assert.Equal(t, rehash("h", ""), "h")
}
