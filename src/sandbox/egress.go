package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/internal/version"
)

// proxyRepo is the published build of the sibling proxy package
// (src/sandbox/proxy) -- the two speak the PROXY_* env protocol. override via
// Egress.Image to run your own build.
const proxyRepo = "ghcr.io/lczyk/pats/egress-proxy"

// ProxyImage resolves a sandbox's egress proxy image and, when it can't pin to a
// version, a warning to surface. a configured proxy-image wins. otherwise the
// default is pinned to this build's pats version (tag `v<version>`, as
// publish_images.yml tags it) -- the proxy's filtering protocol is coupled to
// the binary, so `latest` could be a mismatched newer proxy. if the version
// isn't a clean release (dev/dirty/unset -> no image was published for it), fall
// back to `latest` and return a warning; the caller logs it once.
func ProxyImage(configured string) (image, warn string) {
	if configured != "" {
		return configured, ""
	}
	v := version.Info.Version
	if !releaseVersion(v) {
		return proxyRepo + ":latest", fmt.Sprintf("pats version %q is not a release; using egress-proxy:latest, which may not match this build (set a sandbox proxy-image to pin)", v)
	}
	return proxyRepo + ":v" + v, ""
}

// releaseVersion reports whether v is a bare X.Y.Z (digits only) -- the shape
// that has a published `v<version>` image. anything else (empty, a dev/dirty
// suffix, a pre-release) has none, so we fall back to latest.
func releaseVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return false
		}
	}
	return true
}

// namePrefix marks everything this package creates on the docker side
// (networks, sidecar containers) and the in-container CA mount path, so a
// crashed run is easy to spot and sweep up.
const namePrefix = "sbx-"

// setupEgress applies a Spec's egress policy and returns the extra `docker run`
// args for the agent container, plus a teardown (nil when nothing to tear down).
//
//	open/""    -> open network, no-op
//	none       -> --network none
//	proxy      -> internal network + proxy sidecar; agent reaches the net only
//	              via the proxy, which allow/denies by host and writes a json audit.
//	mitm-proxy -> proxy, plus: hosts named by deny-urls get their tls terminated
//	              with a per-run CA so requests are filtered by full url.
func (c *container) setupEgress(ctx context.Context, spec Spec) (netArgs []string, teardown func(), err error) {
	switch spec.Egress.Mode {
	case "", "open":
		return nil, nil, nil
	case "none":
		return []string{"--network", "none"}, nil, nil
	case "proxy", "mitm-proxy":
		return c.startEgressProxy(ctx, spec)
	default:
		return nil, nil, fmt.Errorf("egress: unknown mode %q", spec.Egress.Mode)
	}
}

func (c *container) startEgressProxy(ctx context.Context, spec Spec) ([]string, func(), error) {
	// unique per run -- the caller gives each run a fresh workdir.
	id := filepath.Base(spec.Workdir)
	net := namePrefix + "egr-" + id
	proxy := namePrefix + "proxy-" + id
	img, _ := ProxyImage(spec.Egress.Image) // warn (if any) is logged by the pre-pull pass

	// internal network: no gateway, so the agent has no direct route out.
	if out, err := c.docker(ctx, "network", "create", "--internal", net); err != nil {
		return nil, nil, fmt.Errorf("egress: create network: %w (%s)", err, out)
	}

	// the audit log is streamed live by a `docker logs -f` follower (started
	// below), not dumped at the end. teardown stops it + removes proxy + network
	// + the mitm CA dir, using a fresh context so it still runs if the agent run
	// was cancelled.
	var logsCmd *exec.Cmd
	var auditF *os.File
	var caDir string
	teardown := func() {
		if logsCmd != nil && logsCmd.Process != nil {
			_ = logsCmd.Process.Kill()
			_ = logsCmd.Wait()
		}
		if auditF != nil {
			_ = auditF.Close()
		}
		_, _ = c.docker(context.Background(), "rm", "-f", proxy)
		_, _ = c.docker(context.Background(), "network", "rm", net)
		if caDir != "" {
			_ = os.RemoveAll(caDir)
		}
	}

	// proxy on the internal net, configured via env.
	penv := []string{
		"-e", "PROXY_DEFAULT=" + orDefault(spec.Egress.Default, "deny"),
		"-e", "PROXY_ALLOW=" + strings.Join(spec.Egress.Allow, ","),
		"-e", "PROXY_DENY=" + strings.Join(spec.Egress.Deny, ","),
	}

	// mitm: per-run CA. key goes to the proxy inline (env, never on disk); the
	// agent gets the cert + a merged trust bundle (image roots + run CA).
	var agentTLSArgs []string
	if spec.Egress.Mode == "mitm-proxy" && len(spec.Egress.DenyURLs)+len(spec.Egress.AllowURLs) > 0 {
		var err error
		if caDir, err = MkTemp(namePrefix + "mitm-ca-"); err != nil {
			teardown()
			return nil, nil, fmt.Errorf("egress: mitm ca dir: %w", err)
		}
		if agentTLSArgs, penv, err = c.setupMitm(ctx, spec, caDir, penv); err != nil {
			teardown()
			return nil, nil, err
		}
	}
	runArgs := append([]string{"run", "-d", "--rm", "--name", proxy, "--network", net}, penv...)
	runArgs = append(runArgs, img)
	if out, err := c.docker(ctx, runArgs...); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: start proxy: %w (%s)", err, out)
	}

	// give the proxy internet via the default bridge (agent stays internal-only).
	if out, err := c.docker(ctx, "network", "connect", "bridge", proxy); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: connect proxy to bridge: %w (%s)", err, out)
	}

	// stream the proxy's audit (one json line per request) live into the file.
	if spec.Egress.AuditPath != "" {
		if f, err := os.Create(spec.Egress.AuditPath); err == nil {
			auditF = f
			logsCmd = exec.Command(c.bin, "logs", "-f", proxy)
			logsCmd.Stdout = f
			logsCmd.Stderr = f
			_ = logsCmd.Start()
		}
	}

	// NOTE: the proxy listens ~instantly; the agent's first egress is seconds
	// away (model round-trip), so we don't poll for readiness. add a wait if a
	// fast first-call ever races startup.
	pURL := "http://" + proxy + ":8080"
	netArgs := []string{
		"--network", net,
		"-e", "HTTP_PROXY=" + pURL, "-e", "HTTPS_PROXY=" + pURL,
		"-e", "http_proxy=" + pURL, "-e", "https_proxy=" + pURL,
	}
	netArgs = append(netArgs, agentTLSArgs...)
	return netArgs, teardown, nil
}

// caBundlePath is where ubuntu/debian images keep the system trust bundle --
// read out of the image to build the merged bundle. NOTE: the merged bundle is
// NOT mounted back over this path: docker desktop injects its own mount there
// (host-CA sharing), so a -v to it fails with "duplicate mount point". the
// bundle lives at a neutral path and env points the tools at it instead.
const caBundlePath = "/etc/ssl/certs/ca-certificates.crt"

// agentBundlePath is where the merged trust bundle is mounted in the agent.
const agentBundlePath = "/sbx-ca/bundle.pem"

// setupMitm generates the per-run CA into caDir, merges it with the agent
// image's trust bundle, and returns the agent-side docker args (mounts + env)
// plus the proxy env extended with the CA + url rules. the CA key stays
// proxy-only -- the agent never sees it.
func (c *container) setupMitm(ctx context.Context, spec Spec, caDir string, penv []string) (agentArgs, proxyEnv []string, err error) {
	certPEM, keyPEM, err := genCA()
	if err != nil {
		return nil, nil, fmt.Errorf("egress: gen mitm ca: %w", err)
	}
	certPath := filepath.Join(caDir, "ca.pem")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, err
	}

	// merged bundle: the image's system roots + the run CA. tunneled (non-mitm)
	// hosts still present real certs, so the system roots must stay in the file.
	bundle, err := c.dockerOut(ctx, "run", "--rm", "--entrypoint", "cat", c.image, caBundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("egress: read image trust bundle %s (mitm-proxy needs it): %w", caBundlePath, err)
	}
	bundlePath := filepath.Join(caDir, "bundle.pem")
	if err := os.WriteFile(bundlePath, append(append(bundle, '\n'), certPEM...), 0o644); err != nil {
		return nil, nil, err
	}

	// cert + key go inline over env, not a mount: the key never touches disk,
	// and the proxy needs no fs access at all -- it runs as a non-root user in
	// a distroless image. env is visible via docker inspect, but that needs
	// docker-socket access, which is root-equivalent anyway.
	proxyEnv = append(penv,
		"-e", "PROXY_DENY_URLS="+strings.Join(spec.Egress.DenyURLs, ","),
		"-e", "PROXY_ALLOW_URLS="+strings.Join(spec.Egress.AllowURLs, ","),
		"-e", "PROXY_CA_CERT="+string(certPEM),
		"-e", "PROXY_CA_KEY="+string(keyPEM),
	)
	agentArgs = []string{
		"-v", bundlePath + ":" + agentBundlePath + ":ro",
		"-v", certPath + ":/sbx-ca/ca.pem:ro",
		// openssl consumers (curl, git) read SSL_CERT_FILE; python requests
		// bundles certifi so it needs its own var; node's var is additive so
		// the run CA alone suffices there. tools honouring none of these fail
		// their tls handshake on mitm'd hosts -- fail closed, not bypassed.
		"-e", "SSL_CERT_FILE=" + agentBundlePath,
		"-e", "CURL_CA_BUNDLE=" + agentBundlePath,
		"-e", "GIT_SSL_CAINFO=" + agentBundlePath,
		"-e", "REQUESTS_CA_BUNDLE=" + agentBundlePath,
		"-e", "NODE_EXTRA_CA_CERTS=/sbx-ca/ca.pem",
	}
	return agentArgs, proxyEnv, nil
}

// dockerOut runs docker with stdout captured alone (no stderr mixed in) -- for
// reading file content out of an image.
func dockerOut(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, bin, args...).Output()
}

func docker(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
