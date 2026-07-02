package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultProxyImage = "pats/egress-proxy:latest"

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
	// unique per pair -- the workdir is a fresh pats-work-* temp dir.
	id := filepath.Base(spec.Workdir)
	net := "pats-egr-" + id
	proxy := "pats-proxy-" + id
	img := spec.Egress.Image
	if img == "" {
		img = defaultProxyImage
	}

	// internal network: no gateway, so the agent has no direct route out.
	if out, err := docker(ctx, c.bin, "network", "create", "--internal", net); err != nil {
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
		_, _ = docker(context.Background(), c.bin, "rm", "-f", proxy)
		_, _ = docker(context.Background(), c.bin, "network", "rm", net)
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

	// mitm: per-run CA. key is mounted into the proxy only; the agent gets the
	// cert + a merged trust bundle (image roots + run CA) over the canonical path.
	var agentTLSArgs []string
	if spec.Egress.Mode == "mitm-proxy" && len(spec.Egress.DenyURLs)+len(spec.Egress.AllowURLs) > 0 {
		var err error
		if caDir, err = os.MkdirTemp("", "pats-mitm-ca-"); err != nil {
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
	if out, err := docker(ctx, c.bin, runArgs...); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: start proxy: %w (%s)", err, out)
	}

	// give the proxy internet via the default bridge (agent stays internal-only).
	if out, err := docker(ctx, c.bin, "network", "connect", "bridge", proxy); err != nil {
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
const agentBundlePath = "/pats-ca/bundle.pem"

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
	keyPath := filepath.Join(caDir, "ca-key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, err
	}

	// merged bundle: the image's system roots + the run CA. tunneled (non-mitm)
	// hosts still present real certs, so the system roots must stay in the file.
	bundle, err := dockerOut(ctx, c.bin, "run", "--rm", "--entrypoint", "cat", c.image, caBundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("egress: read image trust bundle %s (mitm-proxy needs it): %w", caBundlePath, err)
	}
	bundlePath := filepath.Join(caDir, "bundle.pem")
	if err := os.WriteFile(bundlePath, append(append(bundle, '\n'), certPEM...), 0o644); err != nil {
		return nil, nil, err
	}

	proxyEnv = append(penv,
		"-v", caDir+":/pats-ca:ro",
		"-e", "PROXY_DENY_URLS="+strings.Join(spec.Egress.DenyURLs, ","),
		"-e", "PROXY_ALLOW_URLS="+strings.Join(spec.Egress.AllowURLs, ","),
		"-e", "PROXY_CA_CERT=/pats-ca/ca.pem",
		"-e", "PROXY_CA_KEY=/pats-ca/ca-key.pem",
	)
	agentArgs = []string{
		"-v", bundlePath + ":" + agentBundlePath + ":ro",
		"-v", certPath + ":/pats-ca/ca.pem:ro",
		// openssl consumers (curl, git) read SSL_CERT_FILE; python requests
		// bundles certifi so it needs its own var; node's var is additive so
		// the run CA alone suffices there. tools honouring none of these fail
		// their tls handshake on mitm'd hosts -- fail closed, not bypassed.
		"-e", "SSL_CERT_FILE=" + agentBundlePath,
		"-e", "CURL_CA_BUNDLE=" + agentBundlePath,
		"-e", "GIT_SSL_CAINFO=" + agentBundlePath,
		"-e", "REQUESTS_CA_BUNDLE=" + agentBundlePath,
		"-e", "NODE_EXTRA_CA_CERTS=/pats-ca/ca.pem",
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
