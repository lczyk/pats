package sandbox

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestBwrapArgs(t *testing.T) {
	spec := Spec{
		Argv: []string{"sh", "-c", "true"}, // NOT included -- the caller appends it
		Env:  map[string]string{"B": "2", "A": "1"},
		Mounts: []Mount{
			{Host: "/x", Container: "/in/x"},
			{Host: "/y", Container: "/in/y", ReadOnly: true},
		},
	}
	args, err := bwrapArgs(spec, "/work", []string{"--unshare-net"})
	require.NoError(t, err)
	s := strings.Join(args, " ")

	assert.ContainsString(t, s, "--ro-bind / /")
	assert.ContainsString(t, s, "--bind /work "+WorkMount)
	assert.ContainsString(t, s, "--chdir "+WorkMount)
	assert.ContainsString(t, s, "--bind /x /in/x")
	assert.ContainsString(t, s, "--ro-bind /y /in/y")
	// env is sorted (stable argv) and cleared first.
	assert.ContainsString(t, s, "--clearenv")
	assert.ContainsString(t, s, "--setenv A 1 --setenv B 2")
	// extra (egress) args land after everything else.
	assert.That(t, strings.HasSuffix(s, "--unshare-net"), "extra args at the end")
	// the private areas are masked.
	for _, p := range []string{"/home", "/root", "/run", "/tmp"} {
		assert.ContainsString(t, s, "--tmpfs "+p)
	}
	// argv is not in there.
	assert.That(t, !strings.Contains(s, "true"), "argv not included")
}

func TestEgressRule(t *testing.T) {
	r := egressRule(Egress{Default: "allow", Deny: []string{"evil.com"}})
	assert.That(t, r.DefaultAllow, "default allow")
	r = egressRule(Egress{Allow: []string{"good.com"}, DenyURLs: []string{"github.com/x*"}})
	assert.That(t, !r.DefaultAllow, "default deny")
	assert.Len(t, r.DenyURLs, 1)
}

// COVER: forward is the netns-side half of the proxy bridge; exercised here
// w/out a netns (plain tcp -> unix -> echo), since the linux namespace path
// can't run on every dev machine.
func TestForward(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	uln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer uln.Close()
	go func() { // echo server on the unix side
		for {
			c, err := uln.Accept()
			if err != nil {
				return
			}
			go func() {
				b := bufio.NewReader(c)
				line, _ := b.ReadString('\n')
				fmt.Fprintf(c, "echo:%s", line)
				c.Close()
			}()
		}
	}()

	tln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tln.Close()
	go forward(tln, sock)

	c, err := net.Dial("tcp", tln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	fmt.Fprintln(c, "hi")
	line, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, line, "echo:hi\n")
}

func TestSetupBwrapMitm(t *testing.T) {
	// point the host-bundle lookup at a fixture -- macos has none of the real
	// paths, and the test shouldn't depend on the host's trust store anyway.
	dir := t.TempDir()
	fake := filepath.Join(dir, "roots.crt")
	require.NoError(t, os.WriteFile(fake, []byte("# fake system roots"), 0o644))
	orig := hostCABundlePaths
	hostCABundlePaths = []string{fake}
	defer func() { hostCABundlePaths = orig }()

	caDir := t.TempDir()
	signer, err := setupBwrapMitm(caDir)
	require.NoError(t, err)
	assert.NotNil(t, signer)

	cert, err := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	require.NoError(t, err)
	assert.ContainsString(t, string(cert), "BEGIN CERTIFICATE")
	bundle, err := os.ReadFile(filepath.Join(caDir, "bundle.pem"))
	require.NoError(t, err)
	// merged: system roots first, then the run CA.
	assert.ContainsString(t, string(bundle), "# fake system roots")
	assert.ContainsString(t, string(bundle), "BEGIN CERTIFICATE")
}

func TestHostCABundleMissing(t *testing.T) {
	orig := hostCABundlePaths
	hostCABundlePaths = []string{filepath.Join(t.TempDir(), "nope")}
	defer func() { hostCABundlePaths = orig }()
	_, err := hostCABundle()
	assert.Error(t, err, "no system trust bundle")
}
