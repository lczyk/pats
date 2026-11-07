package sandbox

import (
	"context"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu:26.04":                    "docker.io/library/ubuntu:26.04",
		"python":                          "docker.io/library/python:latest",
		"user/img:1":                      "docker.io/user/img:1",
		"ghcr.io/lczyk/pats/sandbox:v1":   "ghcr.io/lczyk/pats/sandbox:v1",
		"localhost:5000/img":              "localhost:5000/img:latest",
		"registry.example.com:5000/a/b:c": "registry.example.com:5000/a/b:c",
	} {
		assert.Equal(t, NormalizeRef(in), want, in)
	}
}

// a tag's digest by a HEAD, following the registry's bearer challenge to an
// anonymous token; a tag the registry lacks is an error.
func TestTagDigest(t *testing.T) {
	r := newFakeRegistry()
	r.challenge = true
	r.index("v1", map[string]string{"amd64": r.image(t, map[string]string{"x": "y"})})
	reg := r.serve(t)
	d, err := reg.tagDigest(context.Background(), "reg.example/org/pats:v1")
	require.NoError(t, err)
	assert.Equal(t, d, r.tags["v1"])
	_, err = reg.tagDigest(context.Background(), "reg.example/org/pats:v2")
	assert.Error(t, err, "404")
}

// an image remote's alias resolves to its fingerprint through image list,
// the alias deciding among prefix matches.
func TestLXDRemoteFingerprint(t *testing.T) {
	f := newFakeServer(t)
	f.bases["ubuntu:26.04.1"] = fakeFP("other")
	l := lxdOn(t, f, Options{})
	fp, err := l.remoteFingerprint(context.Background(), "ubuntu:26.04")
	require.NoError(t, err)
	assert.Equal(t, fp, fakeFP("ubuntu:26.04"))
	_, err = l.remoteFingerprint(context.Background(), "ubuntu:30.04")
	assert.Error(t, err, "not listed")
}
