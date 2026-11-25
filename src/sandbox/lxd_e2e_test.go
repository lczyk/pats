package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
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
		// a group a root step gives the agent user reaches the agent, and
		// its own names stand in the env: plain `lxc exec --user --group`
		// would leave the group list empty.
		code, out = run(t, sess, true, "groupadd pats-e2e && usermod -aG pats-e2e pats")
		assert.Equal(t, code, 0, "%s", out)
		code, out = run(t, sess, false, `id -Gn; echo "$USER $LOGNAME $(id -gn)"`)
		assert.Equal(t, code, 0, "%s", out)
		assert.ContainsString(t, out, "pats-e2e")
		assert.ContainsString(t, out, "pats pats pats\n")
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
			Policy: proxy.Policy{Allow: proxy.Lines("example.com")}}})
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

// placement on a real lxd server (PATS_LXD_E2E=1): a cli's tree is pushed to
// the builder and copied over its root, the image is stamped with its
// version, and a sandbox of the image runs it, root's and on PATH.
func TestLXDE2EPlacesHarness(t *testing.T) {
	if os.Getenv("PATS_LXD_E2E") == "" {
		t.Skip("set PATS_LXD_E2E=1 to drive a real lxd server")
	}
	ctx := context.Background()
	defer Shutdown()
	file := tarGz(t,
		tarEntry{name: "package/", mode: 0o755, typ: tar.TypeDir},
		tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\necho 'tool 1.2.3'\n"},
	)
	d, err := Open(Options{Driver: "lxd"}.WithEnv())
	require.NoError(t, err)
	arch, err := d.Arch(ctx, "")
	require.NoError(t, err)
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: arch, SHA256: "e2e-" + arch, Bin: "package/tool"}

	var log bytes.Buffer
	img, _, built, err := d.Build(ctx, BuildSpec{Base: "ubuntu:24.04", Harness: []Placed{{Artefact: a, File: file}}, Stamp: []string{"tool"}, Role: "sandbox"}, &log)
	require.NoError(t, err, "%v\n%s", err, log.String())
	t.Logf("built %v: %s (%s)", built, img.Name, img.ID)
	t.Cleanup(func() { _ = d.RemoveImage(context.Background(), img.ID) })

	stamps, err := d.Stamps(ctx, img.ID, []string{"tool", "absent"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"tool": "1.2.3"})

	sb, err := d.Sandbox(SandboxOptions{Image: img.Name})
	require.NoError(t, err)
	var out, errb bytes.Buffer
	// the directories above the cli's own stay the base's: root's, 0755.
	const above = "/ /usr /usr/local /usr/local/bin /opt /opt/pats /opt/pats/harness"
	code, err := sb.Run(ctx, Spec{Argv: []string{"sh", "-c", "command -v tool && stat -c '%u:%g %a' /opt/pats/harness/tool/package/tool && readlink /usr/local/bin/tool && tool --version && stat -c '%u:%g %a' " + above + " | sort -u && ls / | grep -c pats-harness"},
		Workdir: t.TempDir(), Egress: Egress{None: true}}, &out, &errb)
	require.NoError(t, err, "%s", errb.String())
	t.Logf("exit %d: %s%s", code, out.String(), errb.String())
	assert.Equal(t, out.String(), "/usr/local/bin/tool\n0:0 755\n/opt/pats/harness/tool/package/tool\ntool 1.2.3\n0:0 755\n0\n")

	// the same inputs again are the same image, with nothing built.
	again, _, built, err := d.Build(ctx, BuildSpec{Base: "ubuntu:24.04", Harness: []Placed{{Artefact: a, File: "/nonexistent/not-read"}}, Stamp: []string{"tool"}, Role: "sandbox"}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, built, false)
	assert.Equal(t, again.ID, img.ID)
}

// an image exported by its alias, removed from the server and found in the
// image cache comes back as it was: under its alias, with its fingerprint
// and the properties its publish set, and nothing is built
// (docs/spec/image-cache.md). the export is the one a ci runs.
func TestLXDE2EImageCache(t *testing.T) {
	if os.Getenv("PATS_LXD_E2E") == "" {
		t.Skip("set PATS_LXD_E2E=1 to drive a real lxd server")
	}
	ctx := context.Background()
	defer Shutdown()
	lxc := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("lxc", args...).CombinedOutput()
		require.NoError(t, err, "lxc %s: %s", strings.Join(args, " "), out)
		return string(out)
	}
	remote := strings.TrimSpace(lxc("remote", "get-default"))
	props := func(fp string) map[string]string {
		var im struct {
			Properties map[string]string `json:"properties"`
		}
		require.NoError(t, json.Unmarshal([]byte(lxc("query", remote+":/1.0/images/"+fp+"?project="+lxdProject)), &im))
		return im.Properties
	}

	// a script no other run builds, so the image is made here and is this
	// test's to remove: by its fingerprint, which an import that never got
	// its alias would still have.
	tag := make([]byte, 6)
	_, err := rand.Read(tag)
	require.NoError(t, err)
	spec := BuildSpec{Base: "ubuntu:24.04", Argv: []string{"sh", "-c", "echo " + hex.EncodeToString(tag) + " > /etc/pats-e2e"}, Role: "project", Sandbox: "e2e-cache"}
	d, err := Open(Options{Driver: "lxd"}.WithEnv())
	require.NoError(t, err)
	var log bytes.Buffer
	img, _, built, err := d.Build(ctx, spec, &log)
	require.NoError(t, err, "%v\n%s", err, log.String())
	require.That(t, built, "the image was there already: not this test's")
	t.Cleanup(func() {
		_, _ = exec.Command("lxc", "image", "delete", remote+":"+img.ID, "--project", lxdProject).CombinedOutput()
	})
	before := props(img.ID)

	dir := t.TempDir()
	lxc("image", "export", remote+":"+img.Name, filepath.Join(dir, img.Name), "--project", lxdProject)
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, ents, 1) // a unified image: one file
	assert.That(t, strings.HasPrefix(ents[0].Name(), img.Name+".tar"), "the export is named %s, not the alias and .tar", ents[0].Name())
	sum, err := fileSHA256(filepath.Join(dir, ents[0].Name()))
	require.NoError(t, err)
	assert.Equal(t, sum, img.ID) // the file's hash is the fingerprint

	lxc("image", "delete", remote+":"+img.Name, "--project", lxdProject)
	cached, err := Open(Options{Driver: "lxd", ImageCache: dir}.WithEnv())
	require.NoError(t, err)
	log.Reset()
	again, _, built, err := cached.Build(ctx, spec, &log)
	require.NoError(t, err, "%v\n%s", err, log.String())
	assert.That(t, !built, "the image was built with its file in the cache:\n%s", log.String())
	assert.Equal(t, again, img)
	after := props(again.ID)
	for _, k := range []string{"pats.inputs", "pats.role", "pats.sandbox", "pats.version", "pats.stamped"} {
		assert.Equal(t, after[k], before[k], k)
		assert.That(t, before[k] != "", "%s was not set at the publish", k)
	}
}
