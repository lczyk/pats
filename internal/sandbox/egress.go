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
//	off/""  -> open network, no-op
//	none    -> --network none
//	proxy   -> internal network + proxy sidecar; agent reaches the net only via
//	           the proxy, which allow/denies by host and writes a json audit.
func (c *container) setupEgress(ctx context.Context, spec Spec) (netArgs []string, teardown func(), err error) {
	switch spec.Egress.Mode {
	case "", "off":
		return nil, nil, nil
	case "none":
		return []string{"--network", "none"}, nil, nil
	case "proxy":
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
	// below), not dumped at the end. teardown stops it + removes proxy + network,
	// using a fresh context so it still runs if the agent run was cancelled.
	var logsCmd *exec.Cmd
	var auditF *os.File
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
	}

	// proxy on the internal net, configured via env.
	penv := []string{
		"-e", "PROXY_DEFAULT=" + orDefault(spec.Egress.Default, "deny"),
		"-e", "PROXY_ALLOW=" + strings.Join(spec.Egress.Allow, ","),
		"-e", "PROXY_DENY=" + strings.Join(spec.Egress.Deny, ","),
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
	return netArgs, teardown, nil
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
