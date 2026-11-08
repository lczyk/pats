package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// TestLXDE2E drives a real lxd server (the lxc cli's default remote) through
// every egress mode. it needs PATS_LXD_E2E=1; on a host that also runs
// docker, the bridges must be let through docker's FORWARD policy first (see
// the readme). it builds a linux pats for the proxy container, so it runs
// where go can build for the server's arch (PATS_LXD_E2E_GOARCH when it is not
// this machine's).
func TestLXDE2E(t *testing.T) {
	if os.Getenv("PATS_LXD_E2E") == "" {
		t.Skip("set PATS_LXD_E2E=1 to drive a real lxd server")
	}
	ctx := context.Background()
	defer Shutdown()
	bin := filepath.Join(t.TempDir(), "pats")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/pats")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if arch := os.Getenv("PATS_LXD_E2E_GOARCH"); arch != "" {
		build.Env = append(build.Env, "GOARCH="+arch) // a server of another arch than this machine
	}
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%v\n%s", err, out)
	o := Options{Driver: "lxd", ProxyBin: bin}.WithEnv()

	d, err := Open(o)
	require.NoError(t, err)
	var log bytes.Buffer
	img, _, _, err := d.Build(ctx, BuildSpec{Base: "ubuntu:24.04"}, &log)
	require.NoError(t, err, "%v\n%s", err, log.String())
	sb, err := d.Sandbox(SandboxOptions{Image: img.Name})
	require.NoError(t, err)

	run := func(t *testing.T, sess Session, root bool, script string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code, err := sess.Exec(ctx, Cmd{Argv: []string{"bash", "-c", script}, Root: root}, &out, &out)
		require.NoError(t, err)
		return code, out.String()
	}

	t.Run("none", func(t *testing.T) {
		work := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(work, "seed"), []byte("host\n"), 0o644))
		sess, err := sb.Start(ctx, Spec{Workdir: work, Egress: Egress{None: true}})
		require.NoError(t, err)
		defer sess.Close()
		code, _ := run(t, sess, true, "echo root > rootfile")
		assert.Equal(t, code, 0)
		require.NoError(t, sess.HandOver(ctx, WorkMount))
		code, out := run(t, sess, false, "id -un; cat seed; echo agent >> rootfile; ip -br link | grep -vc '^lo'")
		assert.Equal(t, code, 1, "%s", out) // grep -c counts no nic but lo, and exits 1
		assert.ContainsString(t, out, "pats\nhost\n0\n")
		require.NoError(t, sess.Sync(ctx))
		b, err := os.ReadFile(filepath.Join(work, "rootfile"))
		require.NoError(t, err)
		assert.Equal(t, string(b), "root\nagent\n")
	})

	t.Run("open", func(t *testing.T) {
		sess, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: Egress{SelfTest: true}})
		require.NoError(t, err)
		defer sess.Close()
		code, out := run(t, sess, false, "timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo internet")
		assert.Equal(t, code, 0, "%s", out)
		// the bridge's own host address is in a private range the acl drops.
		gw := strings.TrimSpace(func() string { _, o := run(t, sess, false, "ip -4 route show default | cut -d' ' -f3"); return o }())
		code, out = run(t, sess, false, "timeout 5 bash -c 'exec 3<>/dev/tcp/"+gw+"/8443' && echo LEAK")
		assert.NotEqual(t, code, 0, "%s", out)
	})

	t.Run("proxy", func(t *testing.T) {
		audit := filepath.Join(t.TempDir(), "egress.log")
		sess, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Sudo: true, Egress: Egress{Set: true, SelfTest: true, AuditPath: audit,
			Policy: proxy.Policy{Allow: []string{"example.com"}}}})
		require.NoError(t, err)
		defer sess.Close()
		script := `p=${HTTP_PROXY#http://}; exec 3<>/dev/tcp/${p%:*}/${p##*:}; printf "GET http://$1/ HTTP/1.1\r\nHost: $1\r\nConnection: close\r\n\r\n" >&3; head -1 <&3`
		for host, want := range map[string]string{"example.com": "200", "evil.test": "403"} {
			var out bytes.Buffer
			_, err := sess.Exec(ctx, Cmd{Argv: []string{"bash", "-c", script, "probe", host}}, &out, &out)
			require.NoError(t, err)
			assert.ContainsString(t, out.String(), want)
		}
		code, out := run(t, sess, false, "timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo LEAK")
		assert.NotEqual(t, code, 0, "%s", out)
		require.NoError(t, sess.Close())
		b, _ := os.ReadFile(audit)
		assert.ContainsString(t, string(b), `"host":"evil.test"`)
	})
}
