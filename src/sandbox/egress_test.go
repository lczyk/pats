package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	if args, td, err := c.setupEgress(context.Background(), Spec{}); err != nil || args != nil || td != nil {
		t.Fatalf("open: got %v %p %v", args, td, err)
	}
	args, _, err := c.setupEgress(context.Background(), Spec{Egress: Egress{Mode: "none"}})
	if err != nil || !slices.Equal(args, []string{"--network", "none"}) {
		t.Fatalf("none: got %v %v", args, err)
	}
	if _, _, err := c.setupEgress(context.Background(), Spec{Egress: Egress{Mode: "wat"}}); err == nil {
		t.Fatal("unknown mode: want error")
	}
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
	if err != nil {
		t.Fatal(err)
	}

	// agent joins the internal net and gets proxy env, upper+lowercase.
	joined := strings.Join(netArgs, " ")
	for _, want := range []string{
		"--network sbx-egr-pats-work-x",
		"HTTP_PROXY=http://sbx-proxy-pats-work-x:8080",
		"https_proxy=http://sbx-proxy-pats-work-x:8080",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("netArgs missing %q in %q", want, joined)
		}
	}

	// internal network (no gateway) + proxy run with the policy env.
	if fd.call("network create --internal sbx-egr-pats-work-x") == nil {
		t.Error("no internal network create")
	}
	run := strings.Join(fd.call("run -d"), " ")
	for _, want := range []string{
		"PROXY_DEFAULT=deny", // empty default resolves to deny
		"PROXY_ALLOW=api.example.com",
		"PROXY_DENY=evil.example.com",
		"--network sbx-egr-pats-work-x",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("proxy run missing %q in %q", want, run)
		}
	}
	if fd.call("network connect bridge") == nil {
		t.Error("proxy not connected to bridge")
	}

	teardown()
	if fd.call("rm -f sbx-proxy-pats-work-x") == nil || fd.call("network rm sbx-egr-pats-work-x") == nil {
		t.Error("teardown did not remove proxy + network")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	run := strings.Join(fd.call("run -d"), " ")
	for _, want := range []string{
		"PROXY_DENY_URLS=github.com/*/secrets*",
		"PROXY_CA_CERT=-----BEGIN",
		"PROXY_CA_KEY=-----BEGIN",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("mitm proxy run missing %q", want)
		}
	}

	// agent side: bundle + cert mounts and the tls env vars, no key anywhere.
	joined := strings.Join(netArgs, " ")
	for _, want := range []string{agentBundlePath + ":ro", "/sbx-ca/ca.pem:ro", "SSL_CERT_FILE=", "NODE_EXTRA_CA_CERTS="} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent args missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "PRIVATE KEY") {
		t.Error("CA key leaked into agent args")
	}

	// the CA dir on disk holds cert + merged bundle only -- never the key.
	var bundlePath string
	for _, a := range netArgs {
		if strings.HasSuffix(a, agentBundlePath+":ro") {
			bundlePath = strings.TrimSuffix(a, ":"+agentBundlePath+":ro")
		}
	}
	if bundlePath == "" {
		t.Fatal("no bundle mount found")
	}
	caDir := filepath.Dir(bundlePath)
	ents, err := os.ReadDir(caDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(caDir, e.Name()))
		if strings.Contains(string(b), "PRIVATE KEY") {
			t.Errorf("CA key on disk in %s", e.Name())
		}
	}
	// merged bundle keeps the image's system roots (tunneled hosts need them).
	b, _ := os.ReadFile(bundlePath)
	if !strings.Contains(string(b), "systemroots") {
		t.Error("bundle lost the image system roots")
	}
}

func TestStartEgressProxyFailureTearsDown(t *testing.T) {
	fd := &fakeDocker{failOn: "run -d"}
	c := &container{bin: "docker", image: "img", exec: fd.exec, execOut: fd.execOut}
	spec := Spec{Workdir: filepath.Join(t.TempDir(), "pats-work-f"), Egress: Egress{Mode: "proxy"}}
	if _, _, err := c.startEgressProxy(context.Background(), spec); err == nil {
		t.Fatal("want error from failed proxy start")
	}
	if fd.call("network rm sbx-egr-pats-work-f") == nil {
		t.Error("failure path did not remove the network")
	}
}
