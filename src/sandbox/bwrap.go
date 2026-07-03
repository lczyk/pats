// bwrap driver: runs the command directly on the (linux) host under
// bubblewrap. no image -- the host filesystem is the rootfs, bound read-only,
// with the private bits (/home, /root, /run, /tmp) masked by tmpfs. the
// process runs as the invoking user, so workdir files come out host-owned
// with no uid juggling.
//
// egress proxy/mitm-proxy has no sidecar: the proxy engine
// (src/sandbox/proxy) runs in-process in pats, listening on a unix socket,
// and the sandbox lives in a fresh network namespace whose only interface is
// loopback. a small helper (this same binary re-exec'd as `pats __sbx-net`,
// see bwrap_linux.go) sits inside the netns forwarding 127.0.0.1:8080 to that
// socket, then runs bwrap. HTTP(S)_PROXY points at the forwarder; anything
// ignoring the proxy env has no route out -- fail closed.
//
// this file is the portable part (argv building, rule mapping, the tcp->unix
// forwarder); the run + netns plumbing is linux-only (bwrap_linux.go, with a
// stub for other platforms).
package sandbox

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// bwrapSandbox runs specs under bubblewrap. constructed by New on linux only.
type bwrapSandbox struct{}

// proxyAddr is where the in-netns forwarder listens; the netns is private to
// one run, so a fixed port cannot collide.
const proxyAddr = "127.0.0.1:8080"

// bwrapArgs builds the bwrap argv (options only -- the caller appends
// spec.Argv). extra carries per-egress-mode additions (--unshare-net, the CA
// bind + tls env, the proxy env).
func bwrapArgs(spec Spec, workdir string, extra []string) ([]string, error) {
	args := []string{
		// the host fs is the rootfs, read-only. private areas are masked below;
		// anything the run legitimately needs from them comes back via Mounts.
		"--ro-bind", "/", "/",
		// NOTE: masking /home, /root and /run hides user secrets (ssh keys,
		// agent sockets, docker.sock) from the sandboxed process; the rest of /
		// stays readable. a tighter allowlist rootfs can replace this later.
		"--tmpfs", "/home",
		"--tmpfs", "/root",
		"--tmpfs", "/run",
		"--tmpfs", "/tmp",
		"--dev", "/dev",
		"--proc", "/proc",
		"--bind", workdir, WorkMount,
		"--chdir", WorkMount,
		"--unshare-pid",
		"--die-with-parent",
		"--new-session",
		"--clearenv",
	}
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			return nil, fmt.Errorf("resolve mount %s: %w", m.Host, err)
		}
		bind := "--bind"
		if m.ReadOnly {
			bind = "--ro-bind"
		}
		args = append(args, bind, mh, m.Container)
	}
	for _, kv := range sortedEnv(spec.Env) {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, "--setenv", k, v)
	}
	return append(args, extra...), nil
}

// egressRule maps a Spec's egress policy to the proxy engine's Rule.
func egressRule(eg Egress) proxy.Rule {
	return proxy.Rule{
		DefaultAllow: orDefault(eg.Default, "deny") == "allow",
		Allow:        eg.Allow,
		Deny:         eg.Deny,
		DenyURLs:     proxy.ParseURLRules(eg.DenyURLs),
		AllowURLs:    proxy.ParseURLRules(eg.AllowURLs),
	}
}

// proxyEnvArgs is the --setenv set pointing the sandboxed process at the
// forwarder (mirrors the docker path's HTTP(S)_PROXY env).
func proxyEnvArgs() []string {
	pURL := "http://" + proxyAddr
	return []string{
		"--setenv", "HTTP_PROXY", pURL, "--setenv", "HTTPS_PROXY", pURL,
		"--setenv", "http_proxy", pURL, "--setenv", "https_proxy", pURL,
	}
}

// mitmTLSArgs binds the CA dir (ca.pem + bundle.pem, staged by setupBwrapMitm)
// at /sbx-ca and points the usual tls env at it -- same contract as the docker
// path (see setupMitm in egress.go).
func mitmTLSArgs(caDir string) []string {
	return []string{
		"--ro-bind", caDir, "/sbx-ca",
		"--setenv", "SSL_CERT_FILE", agentBundlePath,
		"--setenv", "CURL_CA_BUNDLE", agentBundlePath,
		"--setenv", "GIT_SSL_CAINFO", agentBundlePath,
		"--setenv", "REQUESTS_CA_BUNDLE", agentBundlePath,
		"--setenv", "NODE_EXTRA_CA_CERTS", "/sbx-ca/ca.pem",
	}
}

// hostCABundlePaths are the system trust bundles to try, per distro family.
var hostCABundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt", // debian/ubuntu/arch/alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // fedora/rhel
	"/etc/ssl/ca-bundle.pem",             // suse
}

// hostCABundle reads the host system trust bundle -- the bwrap equivalent of
// reading it out of the image (the host fs IS the image here).
func hostCABundle() ([]byte, error) {
	for _, p := range hostCABundlePaths {
		if b, err := os.ReadFile(p); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no system trust bundle found (tried %s); mitm-proxy needs one", strings.Join(hostCABundlePaths, ", "))
}

// setupBwrapMitm stages the per-run CA into caDir (ca.pem + merged bundle.pem,
// what mitmTLSArgs binds at /sbx-ca) and returns a Signer holding the key --
// which never touches disk or env at all here, only this process's memory.
func setupBwrapMitm(caDir string) (*proxy.Signer, error) {
	certPEM, keyPEM, err := genCA()
	if err != nil {
		return nil, fmt.Errorf("egress: gen mitm ca: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "ca.pem"), certPEM, 0o644); err != nil {
		return nil, err
	}
	bundle, err := hostCABundle()
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "bundle.pem"), append(append(bundle, '\n'), certPEM...), 0o644); err != nil {
		return nil, err
	}
	return proxy.NewSigner(string(certPEM), string(keyPEM))
}

// forward accepts on ln and pipes each conn to a fresh dial of the unix
// socket at sock -- the netns-side half of the proxy bridge. returns when ln
// closes.
func forward(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			u, err := net.Dial("unix", sock)
			if err != nil {
				c.Close()
				return
			}
			go pipeConn(u, c)
			pipeConn(c, u)
		}()
	}
}

func pipeConn(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	io.Copy(dst, src)
}
