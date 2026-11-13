package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu:26.04":                         "docker.io/library/ubuntu:26.04",
		"docker.io/ubuntu:26.04":               "docker.io/library/ubuntu:26.04",
		"index.docker.io/x/y":                  "docker.io/x/y:latest",
		"index.docker.io/library/ubuntu:26.04": "docker.io/library/ubuntu:26.04",
		"registry-1.docker.io/ubuntu":          "docker.io/library/ubuntu:latest",
		"python":                               "docker.io/library/python:latest",
		"user/img:1":                           "docker.io/user/img:1",
		"ghcr.io/lczyk/pats/sandbox:v1":        "ghcr.io/lczyk/pats/sandbox:v1",
		"localhost:5000/img":                   "localhost:5000/img:latest",
		"registry.example.com:5000/a/b:c":      "registry.example.com:5000/a/b:c",
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
	assert.Error(t, err, "not found")
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

func TestSplitRefHosts(t *testing.T) {
	for _, ref := range []string{"localhost/a/b:c", "localhost:5000/team/image:latest", "registry:5000/team/image:latest", "reg.example/a:b"} {
		_, _, err := defaultRegistry.repo(ref)
		assert.That(t, err == nil, "%s: %v", ref, err)
	}
	_, _, err := defaultRegistry.repo("team/image:latest")
	assert.Error(t, err, "names no registry host")
}

func TestRepository(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu:26.04":                          "docker.io/library/ubuntu",
		"ubuntu":                                "docker.io/library/ubuntu",
		"docker.io/ubuntu":                      "docker.io/library/ubuntu",
		"ghcr.io/lczyk/pats/sandbox:v1":         "ghcr.io/lczyk/pats/sandbox",
		"ghcr.io/lczyk/pats/sandbox@sha256:abc": "ghcr.io/lczyk/pats/sandbox",
		"localhost:5000/team/img":               "localhost:5000/team/img",
	} {
		assert.Equal(t, Repository(in), want, in)
	}
}

// a remote ref may name its image by fingerprint, or a unique prefix of one.
func TestLXDRemoteFingerprintByFingerprint(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	want := fakeFP("ubuntu:26.04")
	for _, ref := range []string{"ubuntu:" + want, "ubuntu:" + want[:12]} {
		fp, err := l.remoteFingerprint(context.Background(), ref)
		require.NoError(t, err)
		assert.Equal(t, fp, want)
	}
	got, err := l.fingerprint(context.Background(), "ubuntu:"+want)
	require.NoError(t, err)
	assert.Equal(t, got, want)
}

// an image of the server's own named by a unique fingerprint prefix
// resolves, as lxc takes one.
func TestLXDFingerprintPrefixOnServer(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	fp, _, err := l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	got, err := l.fingerprint(context.Background(), fp[:12])
	require.NoError(t, err)
	assert.Equal(t, got, fp)
	_, err = l.fingerprint(context.Background(), "ffffffffffff")
	assert.Error(t, err, "not found")
}

// an image remote lists every architecture and both instance types: the
// one for the server's arch and the type asked for is picked, by its short
// alias or the one with the arch's suffix.
func TestLXDRemoteFingerprintArchAndType(t *testing.T) {
	list := `[` +
		`{"fingerprint":"amd64c","architecture":"x86_64","type":"container","aliases":[{"name":"26.04"},{"name":"26.04/amd64"}]},` +
		`{"fingerprint":"amd64v","architecture":"x86_64","type":"virtual-machine","aliases":[{"name":"26.04"},{"name":"26.04/amd64"}]},` +
		`{"fingerprint":"arm64c","architecture":"aarch64","type":"container","aliases":[{"name":"26.04/arm64"}]},` +
		`{"fingerprint":"arm64v","architecture":"aarch64","type":"virtual-machine","aliases":[{"name":"26.04/arm64"}]}]`
	lookup := func(t *testing.T, arch string, vm bool) (string, error) {
		l, _ := fakeLXD(t, Options{Driver: "lxd", Remote: "srv", VM: vm})
		l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			switch strings.Join(args, " ") {
			case "query srv:/1.0":
				io.WriteString(stdout, `{"environment":{"architectures":["`+arch+`"]}}`)
			case "image list ubuntu: 26.04 --format=json":
				io.WriteString(stdout, list)
			}
			return nil
		})
		return l.remoteFingerprint(context.Background(), "ubuntu:26.04")
	}
	for _, c := range []struct {
		arch string
		vm   bool
		want string
	}{{"x86_64", false, "amd64c"}, {"x86_64", true, "amd64v"}, {"aarch64", false, "arm64c"}, {"aarch64", true, "arm64v"}} {
		got, err := lookup(t, c.arch, c.vm)
		require.NoError(t, err)
		assert.Equal(t, got, c.want, c.arch)
	}
	_, err := lookup(t, "s390x", false)
	assert.Error(t, err, "not listed for s390x container")
}
