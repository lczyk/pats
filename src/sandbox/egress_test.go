package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// fakeDocker records every cli invocation and returns canned results, so the
// egress setup/teardown plumbing runs without docker.
type fakeDocker struct {
	calls  [][]string
	failOn string // substring of the joined args; matching call errors
	out    []byte // execOut payload (the image trust bundle)
}

func (f *fakeDocker) exec(ctx context.Context, bin string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.failOn != "" && strings.Contains(strings.Join(args, " "), f.failOn) {
		return "boom", errors.New("fake docker failure")
	}
	return "", nil
}

func (f *fakeDocker) execOut(ctx context.Context, bin string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out, nil
}

func (f *fakeDocker) call(sub string) []string {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return c
		}
	}
	return nil
}

func TestSetupEgressModes(t *testing.T) {
	c := &container{bin: "docker", image: "img"}
	args, td, err := c.setupEgress(context.Background(), Spec{})
	require.NoError(t, err)
	assert.Nil(t, args)
	assert.Nil(t, td)
	args, _, err = c.setupEgress(context.Background(), Spec{Egress: Egress{Mode: "none"}})
	require.NoError(t, err)
	assert.EqualArrays(t, args, []string{"--network", "none"})
	_, _, err = c.setupEgress(context.Background(), Spec{Egress: Egress{Mode: "wat"}})
	assert.Error(t, err, assert.AnyError)
}

func TestStartEgressProxy(t *testing.T) {
	fd := &fakeDocker{}
	c := &container{bin: "docker", image: "img", exec: fd.exec, execOut: fd.execOut}
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-x"),
		Egress: Egress{
			Mode:  "proxy",
			Allow: []string{"api.example.com"},
			Deny:  []string{"evil.example.com"},
		},
	}
	netArgs, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)

	// agent joins the internal net and gets proxy env, upper+lowercase.
	joined := strings.Join(netArgs, " ")
	for _, want := range []string{
		"--network sbx-egr-pats-work-x",
		"HTTP_PROXY=http://sbx-proxy-pats-work-x:8080",
		"https_proxy=http://sbx-proxy-pats-work-x:8080",
	} {
		assert.ContainsString(t, joined, want)
	}

	// internal network (no gateway) + proxy run with the policy env.
	assert.NotNil(t, fd.call("network create --internal sbx-egr-pats-work-x"))
	run := strings.Join(fd.call("run -d"), " ")
	for _, want := range []string{
		"PROXY_DEFAULT=deny", // empty default resolves to deny
		"PROXY_ALLOW=api.example.com",
		"PROXY_DENY=evil.example.com",
		"--network sbx-egr-pats-work-x",
	} {
		assert.ContainsString(t, run, want)
	}
	assert.NotNil(t, fd.call("network connect bridge"))

	teardown()
	assert.NotNil(t, fd.call("rm -f sbx-proxy-pats-work-x"))
	assert.NotNil(t, fd.call("network rm sbx-egr-pats-work-x"))
}

// COVER: the mitm security invariants -- CA key reaches the proxy over env
// only (never disk), the agent gets cert + merged bundle but not the key, and
// a mid-setup failure still tears down the network.
func TestStartEgressProxyMitm(t *testing.T) {
	fd := &fakeDocker{out: []byte("-----BEGIN CERTIFICATE-----\nsystemroots\n-----END CERTIFICATE-----")}
	c := &container{bin: "docker", image: "img", exec: fd.exec, execOut: fd.execOut}
	spec := Spec{
		Workdir: filepath.Join(t.TempDir(), "pats-work-m"),
		Egress: Egress{
			Mode:     "mitm-proxy",
			DenyURLs: []string{"github.com/*/secrets*"},
		},
	}
	netArgs, teardown, err := c.startEgressProxy(context.Background(), spec)
	require.NoError(t, err)
	defer teardown()

	run := strings.Join(fd.call("run -d"), " ")
	for _, want := range []string{
		"PROXY_DENY_URLS=github.com/*/secrets*",
		"PROXY_CA_CERT=-----BEGIN",
		"PROXY_CA_KEY=-----BEGIN",
	} {
		assert.ContainsString(t, run, want)
	}

	// agent side: bundle + cert mounts and the tls env vars, no key anywhere.
	joined := strings.Join(netArgs, " ")
	for _, want := range []string{agentBundlePath + ":ro", "/sbx-ca/ca.pem:ro", "SSL_CERT_FILE=", "NODE_EXTRA_CA_CERTS="} {
		assert.ContainsString(t, joined, want)
	}
	assert.Equal(t, strings.Contains(joined, "PRIVATE KEY"), false)

	// the CA dir on disk holds cert + merged bundle only -- never the key.
	var bundlePath string
	for _, a := range netArgs {
		if strings.HasSuffix(a, agentBundlePath+":ro") {
			bundlePath = strings.TrimSuffix(a, ":"+agentBundlePath+":ro")
		}
	}
	require.NotEqual(t, bundlePath, "")
	caDir := filepath.Dir(bundlePath)
	ents, err := os.ReadDir(caDir)
	require.NoError(t, err)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(caDir, e.Name()))
		assert.Equal(t, strings.Contains(string(b), "PRIVATE KEY"), false)
	}
	// merged bundle keeps the image's system roots (tunneled hosts need them).
	b, _ := os.ReadFile(bundlePath)
	assert.ContainsString(t, string(b), "systemroots")
}

func TestStartEgressProxyFailureTearsDown(t *testing.T) {
	fd := &fakeDocker{failOn: "run -d"}
	c := &container{bin: "docker", image: "img", exec: fd.exec, execOut: fd.execOut}
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-f"), Egress: Egress{Mode: "proxy"}}
	if _, _, err := c.startEgressProxy(context.Background(), spec); err == nil {
		assert.Error(t, err, assert.AnyError)
	}
	assert.NotNil(t, fd.call("network rm sbx-egr-pats-work-f"))
}
