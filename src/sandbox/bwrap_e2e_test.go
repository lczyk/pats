//go:build !nobwrap

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// the bwrap driver on a real linux machine with bwrap 0.10+: PATS_BWRAP_E2E=1
// runs it, pulling a public ubuntu image (PATS_BWRAP_IMAGE overrides it).
func TestBwrapE2E(t *testing.T) {
	if os.Getenv("PATS_BWRAP_E2E") == "" {
		t.Skip("PATS_BWRAP_E2E=1 runs it, on linux with bwrap 0.10 or later")
	}
	ref := os.Getenv("PATS_BWRAP_IMAGE")
	if ref == "" {
		ref = "docker.io/library/ubuntu:24.04"
	}
	bin := filepath.Join(t.TempDir(), "pats")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/pats")
	build.Env = append(os.Environ(), "CGO_ENABLED=0") // static: the forwarder runs inside
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%s", out)

	ctx := context.Background()
	d := newBwrap(defaultProcess, Options{Driver: BwrapDriver})
	d.pats, d.forwarder = bin, bin
	img, _, err := d.Pull(ctx, ref, false, os.Stderr)
	require.NoError(t, err)
	stamps, err := d.Stamps(ctx, img.ID, allCLIs)
	require.NoError(t, err)
	t.Logf("stamps: %v", stamps)
	sb, err := d.Sandbox(SandboxOptions{Image: img.ID})
	require.NoError(t, err)
	run := func(t *testing.T, s Session, c Cmd) (string, int) {
		t.Helper()
		var b bytes.Buffer
		code, err := s.Exec(ctx, c, &b, &b)
		require.NoError(t, err)
		return b.String(), code
	}

	t.Run("none", func(t *testing.T) {
		work := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(work, "seed"), []byte("seed\n"), 0o644))
		s, err := sb.Start(ctx, Spec{Workdir: work, Egress: Egress{None: true}})
		require.NoError(t, err)
		dir := s.(*bwrapSession).dir
		material := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(material, "script"), []byte("echo pushed\n"), 0o644))
		require.NoError(t, s.Push(ctx, material, "/pats-prepare"))
		o, code := run(t, s, Cmd{Argv: []string{"sh", "-c", "sh /pats-prepare/script; echo root > /opt/made; rm -rf /pats-prepare; id -u"}, Root: true})
		assert.Equal(t, code, 0, o)
		assert.Equal(t, o, "pushed\n0\n")
		o, code = run(t, s, Cmd{Argv: []string{"sh", "-c", "cat /opt/made seed; id -u; ls /pats-prepare 2>&1; ip -o link 2>/dev/null | grep -vc ' lo:' || true"}})
		assert.Equal(t, code, 0, o)
		assert.ContainsString(t, o, "root\nseed\n"+strconv.Itoa(os.Getuid())+"\n")
		assert.ContainsString(t, o, "No such file")
		// a background child that would write later dies with its exec.
		o, code = run(t, s, Cmd{Argv: []string{"sh", "-c", "(sleep 2; touch /workspace/late) >/dev/null 2>&1 & echo started"}})
		assert.Equal(t, code, 0, o)
		time.Sleep(3 * time.Second)
		_, err = os.Stat(filepath.Join(work, "late"))
		assert.That(t, os.IsNotExist(err), "a background child outlived its exec")
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		start := time.Now()
		code, err = s.Exec(cctx, Cmd{Argv: []string{"sleep", "300"}}, io.Discard, io.Discard)
		cancel()
		require.NoError(t, err)
		assert.Equal(t, code, -1)
		assert.That(t, time.Since(start) < 10*time.Second, "cancel took %s", time.Since(start))
		require.NoError(t, s.Close())
		_, err = os.Stat(dir)
		assert.That(t, os.IsNotExist(err), "close left %s", dir)
	})

	t.Run("open", func(t *testing.T) {
		s, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: Egress{SelfTest: true}})
		require.NoError(t, err)
		defer s.Close()
		o, code := run(t, s, Cmd{Argv: []string{"bash", "-c", "timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo internet"}})
		assert.Equal(t, code, 0, o)
	})

	t.Run("proxy", func(t *testing.T) {
		audit := filepath.Join(t.TempDir(), "egress.log")
		s, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, SelfTest: true, AuditPath: audit,
			Policy: proxy.Policy{Allow: []string{"example.com"}}}})
		require.NoError(t, err)
		script := `p=${HTTP_PROXY#http://}; exec 3<>/dev/tcp/${p%:*}/${p##*:}; printf "GET http://$1/ HTTP/1.1\r\nHost: $1\r\nConnection: close\r\n\r\n" >&3; head -1 <&3`
		for host, want := range map[string]string{"example.com": "200", "evil.test": "403"} {
			o, _ := run(t, s, Cmd{Argv: []string{"bash", "-c", script, "probe", host}})
			assert.ContainsString(t, o, want)
		}
		o, code := run(t, s, Cmd{Argv: []string{"bash", "-c", "timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo LEAK"}})
		assert.NotEqual(t, code, 0, o)
		require.NoError(t, s.Close())
		b, _ := os.ReadFile(audit)
		assert.ContainsString(t, string(b), `"host":"evil.test"`)
	})
}

// placement under bwrap on a real machine (PATS_BWRAP_E2E=1): a cli's tree is
// unpacked into the store once, a session of the image with the tree over it
// runs the cli from PATH, the stamps of the two say its version, and what a
// session writes over the tree stays the session's.
func TestBwrapE2EPlacesHarness(t *testing.T) {
	if os.Getenv("PATS_BWRAP_E2E") == "" {
		t.Skip("PATS_BWRAP_E2E=1 runs it, on linux with bwrap 0.10 or later")
	}
	ref := os.Getenv("PATS_BWRAP_IMAGE")
	if ref == "" {
		ref = "docker.io/library/ubuntu:24.04"
	}
	ctx := context.Background()
	d := newBwrap(defaultProcess, Options{Driver: BwrapDriver})
	img, _, err := d.Pull(ctx, ref, false, os.Stderr)
	require.NoError(t, err)
	arch, err := d.Arch(ctx, "")
	require.NoError(t, err)
	const body = "#!/bin/sh\necho 'tool 1.2.3'\n"
	file := tarGz(t,
		tarEntry{name: "package/", mode: 0o755, typ: tar.TypeDir},
		tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: body},
	)
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: arch, SHA256: "e2e-" + strconv.Itoa(os.Getpid()), Bin: "package/tool"}
	st, err := d.store()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(st.harnessTree(a.TreeID())) })

	ids, err := d.PlaceTrees(ctx, []Placed{{Artefact: a, File: file}})
	require.NoError(t, err)
	assert.Equal(t, d.MissingTree(ids), "")

	stamps, err := d.StampsOver(ctx, img.ID, ids, []string{"tool", "absent"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"tool": "1.2.3"})
	// the image alone has no such cli, and was not told it has.
	bare, err := d.Stamps(ctx, img.ID, []string{"tool"})
	require.NoError(t, err)
	assert.Len(t, bare, 0)

	sb, err := d.Sandbox(SandboxOptions{Image: img.ID, Trees: ids})
	require.NoError(t, err)
	var out, errb bytes.Buffer
	script := "command -v tool && readlink /usr/local/bin/tool && tool --version && echo scribble >> /opt/pats/harness/tool/package/tool; echo wrote=$?"
	_, err = sb.Run(ctx, Spec{Argv: []string{"sh", "-c", script}, Workdir: t.TempDir(), Egress: Egress{None: true}}, &out, &errb)
	require.NoError(t, err, "%s", errb.String())
	t.Logf("%s%s", out.String(), errb.String())
	assert.ContainsString(t, out.String(), "/usr/local/bin/tool\n/opt/pats/harness/tool/package/tool\ntool 1.2.3\n")
	kept, err := os.ReadFile(filepath.Join(st.harnessTree(ids[0]), "opt/pats/harness/tool/package/tool"))
	require.NoError(t, err)
	assert.Equal(t, string(kept), body)

	// a tree a clean took: the session does not start, and says what to run.
	require.NoError(t, os.RemoveAll(st.harnessTree(ids[0])))
	_, err = sb.Run(ctx, Spec{Argv: []string{"true"}, Workdir: t.TempDir(), Egress: Egress{None: true}}, io.Discard, io.Discard)
	assert.Error(t, err, "is not in the store; run pats init")
}
