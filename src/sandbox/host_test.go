package sandbox

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// hostPats is a pats binary built once for the tests that run the egress
// proxy as a child.
var hostPats = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "pats-host-test-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "pats")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/pats").CombinedOutput()
	if err != nil {
		return "", &exec.ExitError{Stderr: out}
	}
	return bin, nil
})

func hostOn(t *testing.T, o Options) *hostSandbox {
	t.Helper()
	h := newHost(testProcess(t), o)
	return &hostSandbox{h: h}
}

func hostRun(t *testing.T, s Session, c Cmd) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code, err := s.Exec(context.Background(), c, &out, &out)
	require.NoError(t, err)
	return out.String(), code
}

// the agent gets the env pats builds and the few names it takes from the
// host, never anything else pats inherited; PATH without relative entries.
func TestHostEnv(t *testing.T) {
	t.Setenv("PATS_HOST_CANARY", "leaked")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("PATH", "relative/bin:"+os.Getenv("PATH")+"::.")
	t.Setenv("LC_ALL", "C")
	b := hostOn(t, Options{Driver: HostDriver})
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	defer sess.Close()
	out, code := hostRun(t, sess, Cmd{Argv: []string{"/usr/bin/env"}, Env: map[string]string{"MINE": "1"}})
	assert.Equal(t, code, 0)
	assert.That(t, !strings.Contains(out, "PATS_HOST_CANARY"), "a pats env var reached the agent: %s", out)
	assert.That(t, !strings.Contains(out, "SSH_AUTH_SOCK"), "the ssh agent reached the agent: %s", out)
	assert.ContainsString(t, out, "MINE=1")
	assert.ContainsString(t, out, "LC_ALL=C")
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "PATH="); ok {
			for _, d := range filepath.SplitList(p) {
				assert.That(t, filepath.IsAbs(d), "relative PATH entry %q", d)
			}
		}
	}
}

// under host an agent is told the dirs themselves, and a prepare runs from
// a path holding shell metacharacters.
func TestHostPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // macos's /var is /private/var to pwd
	require.NoError(t, err)
	odd := filepath.Join(root, "a $x `y` 'q\"")
	work, home := filepath.Join(odd, "work"), filepath.Join(odd, "home")
	require.NoError(t, os.MkdirAll(work, 0o755))
	require.NoError(t, os.MkdirAll(home, 0o755))
	b := hostOn(t, Options{Driver: HostDriver})
	p := PathsOf(b, work, home)
	assert.Equal(t, p.Work, work)
	assert.Equal(t, p.Home, home)
	assert.That(t, strings.HasPrefix(p.Prepare, home), "prepare %q outside the home", p.Prepare)
	assert.Equal(t, PathsOf(&container{}, work, home), Paths{Work: WorkMount, Home: HomeMount, Prepare: PrepareMount})

	sess, err := b.Start(context.Background(), Spec{Workdir: work})
	require.NoError(t, err)
	defer sess.Close()
	material := filepath.Join(root, "material")
	require.NoError(t, os.MkdirAll(filepath.Join(material, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(material, "bin", "helper"), []byte("#!/bin/sh\necho helped\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(material, "script"), []byte("helper; pwd\n"), 0o644))
	require.NoError(t, sess.Push(context.Background(), material, p.Prepare))
	// what eval's inPath wraps a prepare in: the mount as an argument.
	argv := []string{"/bin/sh", "-c", `PATH="$1/bin:$PATH"; export PATH; shift; exec "$@"`, "step", p.Prepare,
		"/bin/sh", "-e", p.Prepare + "/script"}
	out, code := hostRun(t, sess, Cmd{Argv: argv, Root: true})
	assert.Equal(t, code, 0, out)
	assert.Equal(t, out, "helped\n"+work+"\n")
}

// a command's background children end with it, and a cancel ends the
// group; a context done before the start is a launch error.
func TestHostExecGroup(t *testing.T) {
	b := hostOn(t, Options{Driver: HostDriver})
	work := t.TempDir()
	sess, err := b.Start(context.Background(), Spec{Workdir: work})
	require.NoError(t, err)
	defer sess.Close()
	pidFile := filepath.Join(work, "child.pid")
	out, code := hostRun(t, sess, Cmd{Argv: []string{"/bin/sh", "-c", `sleep 300 & echo $! > child.pid; exit 3`}})
	assert.Equal(t, code, 3, out)
	assertGone(t, pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	code, err = sess.Exec(ctx, Cmd{Argv: []string{"/bin/sh", "-c", `sleep 300 & echo $! > child.pid; wait`}}, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, code, -1)
	assertGone(t, pidFile)

	done, stop := context.WithCancel(context.Background())
	stop()
	_, err = sess.Exec(done, Cmd{Argv: []string{"true"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
	_, err = b.Start(done, Spec{Workdir: work})
	assert.ErrorIs(t, err, ErrLaunch)
}

// assertGone waits briefly for the pid in file to be gone.
func assertGone(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	pid := strings.TrimSpace(string(b))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("kill", "-0", pid).Run() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child %s outlived its command", pid)
}

// advisory egress: the agent is handed the proxy, a request through it is
// audited and judged by the policy, one that goes around it is not seen.
func TestHostEgress(t *testing.T) {
	bin, err := hostPats()
	require.NoError(t, err)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin")
	}))
	defer origin.Close()
	host := strings.TrimPrefix(origin.URL, "http://")
	b := hostOn(t, Options{Driver: HostDriver})
	b.h.pats = bin
	audit := filepath.Join(t.TempDir(), "egress.log")
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, AuditPath: audit,
		Policy: proxy.Policy{AllowPrivate: true, Allow: []string{host}, Deny: []string{"evil.test"}}}})
	require.NoError(t, err)
	s := sess.(*hostSession)
	assert.ContainsString(t, s.env["HTTP_PROXY"], "http://127.0.0.1:")
	pid := s.proxy.Process.Pid
	proxyURL, err := url.Parse(s.env["HTTP_PROXY"])
	require.NoError(t, err)
	get := func(target string, viaProxy bool) int {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
		if viaProxy {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
		resp, err := client.Get(target)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, get(origin.URL+"/", true), 200)
	assert.Equal(t, get("http://evil.test/", true), 403)
	assert.Equal(t, get(origin.URL+"/around", false), 200) // advisory: nothing stops it
	require.NoError(t, sess.Close())
	b2, _ := os.ReadFile(audit)
	assert.ContainsString(t, string(b2), `"host":"evil.test"`)
	assert.That(t, !strings.Contains(string(b2), "/around"), "a request around the proxy was audited: %s", b2)
	assert.That(t, exec.Command("kill", "-0", strconv.Itoa(pid)).Run() != nil, "proxy %d outlived close", pid)

	// tls inspection needs a bundle the user vouches for.
	_, err = b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true,
		Policy: proxy.Policy{Allow: []string{"example.com/x/*"}}}})
	assert.Error(t, err, "SSL_CERT_FILE")

	// a proxy that dies before it listens fails the start.
	b.h.pats = "/usr/bin/false"
	if _, err := os.Stat(b.h.pats); err != nil {
		b.h.pats = "/bin/false"
	}
	_, err = b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true,
		Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	assert.Error(t, err, "exited before listening")

	// one that never listens fails it too, and is reaped, not left running.
	stall := filepath.Join(t.TempDir(), "stall")
	pidFile := stall + ".pid"
	require.NoError(t, os.WriteFile(stall, []byte("#!/bin/sh\necho $$ > '"+pidFile+"'\nexec sleep 300\n"), 0o755))
	b.h.pats = stall
	old := hostProxyReady
	hostProxyReady = 500 * time.Millisecond
	defer func() { hostProxyReady = old }()
	_, err = b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true,
		Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	assert.Error(t, err, "not listening after")
	assertGone(t, pidFile)
}

// an exec's own PATH wins over the inherited one, for the lookup too.
func TestHostEnvOverride(t *testing.T) {
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "only-here"), []byte("#!/bin/sh\necho here\n"), 0o755))
	b := hostOn(t, Options{Driver: HostDriver})
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	defer sess.Close()
	out, code := hostRun(t, sess, Cmd{Argv: []string{"only-here"}, Env: map[string]string{"PATH": tools + ":/usr/bin:/bin"}})
	assert.Equal(t, code, 0)
	assert.Equal(t, out, "here\n")
}

// with no policy, a host agent behind the machine's proxy gets that proxy's
// env (and SSL_CERT_FILE's trust env) passed through.
func TestHostMachinePassthrough(t *testing.T) {
	c, err := proxy.NewChain("http://u:secret@proxy.corp:3128", "", "")
	require.NoError(t, err)
	b := hostOn(t, Options{Driver: HostDriver, Machine: &MachineProxy{Chain: c}, TrustFile: "/etc/corp.pem"})
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	defer sess.Close()
	out, _ := hostRun(t, sess, Cmd{Argv: []string{"/usr/bin/env"}})
	assert.ContainsString(t, out, "HTTPS_PROXY=http://u:secret@proxy.corp:3128")
	assert.ContainsString(t, out, "SSL_CERT_FILE=/etc/corp.pem")
	// a loopback upstream is reachable from a host agent: no refusal.
	c, err = proxy.NewChain("http://127.0.0.1:3128", "", "")
	require.NoError(t, err)
	b = hostOn(t, Options{Driver: HostDriver, Machine: &MachineProxy{Chain: c}})
	sess, err = b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, sess.Close())
}

// close ends what still runs and refuses what comes after.
func TestHostCloseEndsExecs(t *testing.T) {
	b := hostOn(t, Options{Driver: HostDriver})
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	done := make(chan int, 1)
	go func() {
		code, _ := sess.Exec(context.Background(), Cmd{Argv: []string{"sleep", "300"}}, io.Discard, io.Discard)
		done <- code
	}()
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, sess.Close())
	select {
	case code := <-done:
		assert.Equal(t, code, -1)
	case <-time.After(5 * time.Second):
		t.Fatal("an exec outlived close")
	}
	_, err = sess.Exec(context.Background(), Cmd{Argv: []string{"true"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
}

// a command is found on the agent's PATH, the filtered one, never on a
// relative entry; one nowhere on it is a launch error.
func TestHostLookIn(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "tools")
	require.NoError(t, os.MkdirAll(rel, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rel, "sh"), []byte("#!/bin/false\n"), 0o755))
	t.Chdir(dir)
	t.Setenv("PATH", "tools:"+os.Getenv("PATH"))
	b := hostOn(t, Options{Driver: HostDriver})
	sess, err := b.Start(context.Background(), Spec{Workdir: dir})
	require.NoError(t, err)
	defer sess.Close()
	out, code := hostRun(t, sess, Cmd{Argv: []string{"sh", "-c", "echo ran"}})
	assert.Equal(t, code, 0)
	assert.Equal(t, out, "ran\n")
	_, err = sess.Exec(context.Background(), Cmd{Argv: []string{"pats-no-such-cli"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, ErrLaunch)
}

// a hosts-only policy keeps the machine's trust in the agent's env: an
// upstream that inspects tls needs it.
func TestHostHostsOnlyTrust(t *testing.T) {
	bin, err := hostPats()
	require.NoError(t, err)
	b := hostOn(t, Options{Driver: HostDriver, TrustFile: "/etc/corp.pem"})
	b.h.pats = bin
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true,
		Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	require.NoError(t, err)
	defer sess.Close()
	assert.Equal(t, sess.(*hostSession).env["SSL_CERT_FILE"], "/etc/corp.pem")
}

// a proxy that dies while the agent runs fails the command at once, saying
// so, rather than leaving it to a deadline.
func TestHostProxyDeathFailsExec(t *testing.T) {
	bin, err := hostPats()
	require.NoError(t, err)
	b := hostOn(t, Options{Driver: HostDriver})
	b.h.pats = bin
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true,
		Policy: proxy.Policy{Allow: []string{"example.com"}}}})
	require.NoError(t, err)
	defer sess.Close()
	s := sess.(*hostSession)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = s.proxy.Process.Kill()
	}()
	start := time.Now()
	_, err = sess.Exec(context.Background(), Cmd{Argv: []string{"sleep", "300"}}, io.Discard, io.Discard)
	assert.ErrorIs(t, err, errHostProxyDied)
	assert.That(t, time.Since(start) < 10*time.Second, "the exec waited %s after the proxy died", time.Since(start))
}

// a relative SSL_CERT_FILE reaches the agent as an absolute path.
func TestHostRelativeTrustFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	c, err := proxy.NewChain("http://proxy.corp:3128", "", "")
	require.NoError(t, err)
	b := hostOn(t, Options{Driver: HostDriver, Machine: &MachineProxy{Chain: c}, TrustFile: "certs/corp.pem"})
	sess, err := b.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	defer sess.Close()
	want, _ := filepath.Abs("certs/corp.pem")
	assert.Equal(t, sess.(*hostSession).env["SSL_CERT_FILE"], want)
}
