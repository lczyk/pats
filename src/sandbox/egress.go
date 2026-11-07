package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
)

// namePrefix marks everything this package creates on the docker side
// (networks, sidecar containers) and the in-container CA mount path, so a
// crashed run is easy to spot and sweep up.
const namePrefix = "sbx-"

// agentName names the agent container after its workdir, as the sidecar is,
// so `pats clean` can find one that outlived a killed pats (a dead client
// leaves it running). only a workdir MkTemp made is unique by its base name,
// and only its owner file says whether the run is gone: others go unnamed.
func agentName(workdir string) string {
	root, err := TempRoot()
	if err != nil || filepath.Dir(workdir) != root {
		return ""
	}
	return namePrefix + "agent-" + filepath.Base(workdir)
}

// setupEgress applies a Spec's egress policy and returns the extra `docker run`
// args for the agent container, plus a teardown (nil when nothing to tear down).
//
//	open  -> open network, no-op
//	none  -> --network none
//	proxy -> internal network + proxy sidecar; agent reaches the net only via
//	         the proxy, which filters by the policy and writes a json audit.
func (c *container) setupEgress(ctx context.Context, spec Spec) (netArgs []string, teardown func(), err error) {
	switch {
	case spec.Egress.None:
		return []string{"--network", "none"}, nil, nil
	case spec.Egress.Set:
		return c.startEgressProxy(ctx, spec)
	default:
		return nil, nil, nil
	}
}

// material is the driver-neutral half of a proxy-mode setup: what the proxy
// is told and what the sandboxed process is told, before any driver decides
// how the two meet. CA key material is held in memory only; the cert and the
// merged trust bundle go to caDir for the driver to mount.
type material struct {
	policyYAML string
	caCert     string // pem, "" without mitm
	caKey      string // pem, never written to disk
	caDir      string // "" without mitm
	bundlePath string // merged image roots + run CA, under caDir
	certPath   string // run CA cert, under caDir
}

// prepare renders the policy and, when it terminates tls anywhere, mints the
// run CA and merges it into the image's trust bundle (readBundle fetches that
// bundle from the driver). the caller removes caDir.
func prepare(eg Egress, readBundle func() ([]byte, error)) (_ *material, err error) {
	y, err := yaml.Marshal(eg.Policy)
	if err != nil {
		return nil, err
	}
	m := &material{policyYAML: string(y)}
	defer func() {
		if err != nil {
			m.cleanup()
		}
	}()
	if !eg.Policy.NeedsCA() {
		return m, nil
	}
	certPEM, keyPEM, err := GenCA()
	if err != nil {
		return nil, fmt.Errorf("egress: gen mitm ca: %w", err)
	}
	m.caCert, m.caKey = string(certPEM), string(keyPEM)
	if m.caDir, err = MkTemp(namePrefix + "mitm-ca-"); err != nil {
		return nil, fmt.Errorf("egress: mitm ca dir: %w", err)
	}
	m.certPath = filepath.Join(m.caDir, "ca.pem")
	if err := os.WriteFile(m.certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	// merged bundle: the image's system roots + the run CA. tunneled (non-mitm)
	// hosts still present real certs, so the system roots must stay in the file.
	bundle, err := readBundle()
	if err != nil {
		return nil, fmt.Errorf("egress: read image trust bundle %s (url entries need it): %w", caBundlePath, err)
	}
	m.bundlePath = filepath.Join(m.caDir, "bundle.pem")
	if err := os.WriteFile(m.bundlePath, append(append(bundle, '\n'), certPEM...), 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// cleanup removes the CA dir, if any.
func (m *material) cleanup() {
	if m != nil && m.caDir != "" {
		_ = RemoveTemp(m.caDir)
	}
}

// proxyEnv is what the proxy process reads: the policy, and with mitm the CA
// (inline, so the key never touches disk) and where the trust bundle is.
func (m *material) proxyEnv(rootsPath string) map[string]string {
	env := map[string]string{"PATS_EGRESS_POLICY": m.policyYAML}
	if m.caCert != "" {
		env["PATS_EGRESS_CA_CERT"] = m.caCert
		env["PATS_EGRESS_CA_KEY"] = m.caKey
		env["PATS_EGRESS_ROOTS"] = rootsPath
	}
	return env
}

// caBundlePath is where ubuntu/debian images keep the system trust bundle --
// read out of the image to build the merged bundle. NOTE: the merged bundle is
// NOT mounted back over this path: docker desktop injects its own mount there
// (host-CA sharing), so a -v to it fails with "duplicate mount point". the
// bundle lives at a neutral path and env points the tools at it instead.
const caBundlePath = "/etc/ssl/certs/ca-certificates.crt"

// agentBundlePath is where the merged trust bundle is mounted in the agent.
const agentBundlePath = "/sbx-ca/bundle.pem"

// agentCertPath is where the run CA cert alone is mounted in the agent.
const agentCertPath = "/sbx-ca/ca.pem"

// agentEnv is the sandboxed process's side of the contract: the proxy url,
// and with mitm the trust env for every tls stack a tool may use. openssl
// consumers (curl, git) read SSL_CERT_FILE; python requests bundles certifi
// so it needs its own var; node's var is additive so the run CA alone
// suffices there; cargo ignores all of those. tools honouring none of these
// fail their tls handshake on mitm'd hosts -- fail closed, not bypassed.
func (m *material) agentEnv(proxyURL string) map[string]string {
	env := map[string]string{
		"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL,
		"http_proxy": proxyURL, "https_proxy": proxyURL,
	}
	if m.caCert != "" {
		env["SSL_CERT_FILE"] = agentBundlePath
		env["CURL_CA_BUNDLE"] = agentBundlePath
		env["GIT_SSL_CAINFO"] = agentBundlePath
		env["REQUESTS_CA_BUNDLE"] = agentBundlePath
		env["CARGO_HTTP_CAINFO"] = agentBundlePath
		env["NODE_EXTRA_CA_CERTS"] = agentCertPath
	}
	return env
}

// pool is the range pats carves sandbox networks from, one /29 per pair
// (gateway, proxy, agent). docker's own pool is a handful of /16s and runs
// out under -j. PATS_EGRESS_POOL overrides it for the host whose lan is here;
// a bad override fails every proxy run rather than silently using the default.
var pool, poolErr = parsePool(os.Getenv("PATS_EGRESS_POOL"))

func parsePool(v string) (netip.Prefix, error) {
	if v == "" {
		return netip.MustParsePrefix("10.213.0.0/16"), nil
	}
	p, err := netip.ParsePrefix(v)
	if err != nil || !p.Addr().Is4() || p.Bits() > 24 {
		return netip.Prefix{}, fmt.Errorf("PATS_EGRESS_POOL %q: want an ipv4 cidr of /24 or wider", v)
	}
	return p.Masked(), nil
}

const slotBits = 29

// slot hands out the next /29 of the pool. it only orders attempts within
// this process: another pats on the same daemon, or a network a crashed run
// left behind, makes `network create` fail, and the caller moves on.
var slot atomic.Uint32

func nextSubnet() netip.Prefix {
	n := slot.Add(1) - 1
	count := uint32(1) << (slotBits - pool.Bits())
	base := pool.Addr().As4()
	off := (n % count) << (32 - slotBits)
	ip := netip.AddrFrom4([4]byte{
		base[0] | byte(off>>24), base[1] | byte(off>>16), base[2] | byte(off>>8), base[3] | byte(off),
	})
	return netip.PrefixFrom(ip, slotBits)
}

// proxyPort is where the sidecar listens.
const proxyPort = "8080"

// subnetTries bounds how many /29s a pair tries before giving up.
const subnetTries = 64

// readyWait bounds how long the sidecar gets to print its listening line.
const readyWait = 15 * time.Second

func (c *container) startEgressProxy(ctx context.Context, spec Spec) ([]string, func(), error) {
	// unique per run -- the caller gives each run a fresh workdir.
	id := filepath.Base(spec.Workdir)
	net := namePrefix + "egr-" + id
	proxyName := namePrefix + "proxy-" + id
	img, _ := ProxyImage(spec.Egress.Image) // warn (if any) is logged by the pre-pull pass

	subnet, err := c.createNetwork(ctx, net)
	if err != nil {
		return nil, nil, err
	}

	// the audit log is streamed live by a `docker logs -f` follower (started
	// below), not dumped at the end. teardown stops it + removes proxy + network
	// + the mitm CA dir, using a fresh context so it still runs if the agent run
	// was cancelled.
	var logsCmd *exec.Cmd
	var auditF *os.File
	var m *material
	teardown := func() {
		// remove the proxy container first: `docker logs -f` reaches EOF and
		// the follower exits on its own once the container is gone, instead
		// of being killed mid-read. killing the follower first could drop an
		// audit line still in flight between the daemon and the follower --
		// and denied-host lines are the run's cheat detector, so losing the
		// tail matters. safe to call even if the proxy never started: the cli
		// errors on the missing container and we ignore that.
		_, _ = c.docker(context.Background(), "rm", "-f", proxyName)

		if logsCmd != nil && logsCmd.Process != nil {
			done := make(chan error, 1)
			go func() { done <- logsCmd.Wait() }()
			select {
			case <-done: // saw EOF on its own once the container went away
			case <-time.After(2 * time.Second):
				_ = logsCmd.Process.Kill()
				<-done
			}
		}
		if auditF != nil {
			_ = auditF.Close()
		}
		_, _ = c.docker(context.Background(), "network", "rm", net)
		m.cleanup()
	}

	if spec.Egress.SelfTest {
		if err := c.egressCanary(ctx, net); err != nil {
			teardown()
			return nil, nil, err
		}
	}

	m, err = prepare(spec.Egress, func() ([]byte, error) {
		return c.dockerOut(ctx, "run", "--rm", "--entrypoint", "cat", c.image, caBundlePath)
	})
	if err != nil {
		teardown()
		return nil, nil, err
	}

	// the proxy sits at a fixed address on the internal net (the gateway takes
	// the first), so the agent needs no resolver to find it.
	proxyIP := subnet.Addr().Next().Next()
	runArgs := []string{"run", "-d", "--rm", "--name", proxyName, "--network", net, "--ip", proxyIP.String(),
		// the sidecar also sits on the default network, where a container
		// could hand-craft frames at it to be routed out: it forwards nothing
		// and needs no capability to listen on an unprivileged port.
		"--sysctl", "net.ipv4.ip_forward=0", "--sysctl", "net.ipv6.conf.all.forwarding=0",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
	}
	// the proxy sidecar is rootless and reads its config from env; the trust
	// bundle is the one thing too big for env, so it is mounted read-only.
	if m.caCert != "" {
		runArgs = append(runArgs, "-v", m.bundlePath+":"+agentBundlePath+":ro")
	}
	penv := m.proxyEnv(agentBundlePath)
	penv["PATS_EGRESS_REFUSE"] = pool.String()
	// on the internal address only: on :8080 any container on the default
	// network could use this pair's policy and write into its audit.
	penv["PATS_EGRESS_LISTEN"] = proxyIP.String() + ":" + proxyPort
	for _, kv := range sortedEnv(penv) {
		runArgs = append(runArgs, "-e", kv)
	}
	runArgs = append(runArgs, img)
	if out, err := c.docker(ctx, runArgs...); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: start proxy: %w (%s)", err, out)
	}

	// give the proxy internet via the driver's default network (agent stays
	// internal-only).
	if out, err := c.docker(ctx, "network", "connect", c.defaultNetwork(), proxyName); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: connect proxy to %s: %w (%s)", c.defaultNetwork(), err, out)
	}

	// follow the proxy's output: stdout is the audit (one json line per
	// request), stderr carries the readiness line and is otherwise dropped.
	var audit io.Writer = io.Discard
	if spec.Egress.AuditPath != "" {
		f, err := os.Create(spec.Egress.AuditPath)
		if err != nil {
			teardown()
			return nil, nil, fmt.Errorf("egress: audit log: %w", err)
		}
		auditF, audit = f, f
	}
	logsCmd = exec.Command(c.bin, "logs", "-f", proxyName)
	logsCmd.Stdout = audit
	stderr, err := logsCmd.StderrPipe()
	if err != nil {
		teardown()
		return nil, nil, err
	}
	if err := logsCmd.Start(); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: follow proxy logs: %w", err)
	}
	if err := waitReady(stderr, readyWait); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: proxy %s: %w", proxyName, err)
	}

	netArgs := []string{"--network", net}
	if c.driver == "docker" {
		// docker's embedded resolver on an internal network may still forward
		// external names to the host; with no upstream it cannot. peer names
		// (none needed, the proxy is an ip) keep resolving.
		netArgs = append(netArgs, "--dns", "0.0.0.0")
	}
	if m.caCert != "" {
		netArgs = append(netArgs,
			"-v", m.bundlePath+":"+agentBundlePath+":ro",
			"-v", m.certPath+":"+agentCertPath+":ro",
		)
	}
	for _, kv := range sortedEnv(m.agentEnv("http://" + proxyIP.String() + ":" + proxyPort)) {
		netArgs = append(netArgs, "-e", kv)
	}
	return netArgs, teardown, nil
}

// createNetwork makes the pair's internal network on the next free /29 of the
// pool, moving on when the daemon already has that range (another pats, or a
// network a crashed run left behind).
func (c *container) createNetwork(ctx context.Context, name string) (netip.Prefix, error) {
	if poolErr != nil {
		return netip.Prefix{}, poolErr
	}
	var last string
	for range subnetTries {
		subnet := nextSubnet()
		args := []string{"network", "create", "--internal"}
		if c.driver == "docker" {
			// docker gives the host an address on the bridge, which the agent
			// could reach any host service through: --internal cuts only routes.
			args = append(args, "-o", "com.docker.network.bridge.inhibit_ipv4=true")
		}
		args = append(args, "--subnet", subnet.String())
		if c.driver == "podman" {
			// podman's resolver on an internal network answers only for peers,
			// and the sidecar would inherit it as its only nameserver: it could
			// then resolve nothing upstream. nothing here needs peer names.
			args = append(args, "--disable-dns")
		}
		out, err := c.docker(ctx, append(args, name)...)
		if err == nil {
			return subnet, nil
		}
		last = out
		if !subnetTaken(out) {
			return netip.Prefix{}, fmt.Errorf("egress: create network: %w (%s)", err, out)
		}
	}
	return netip.Prefix{}, fmt.Errorf("egress: no free subnet in %s after %d tries (%s); `pats clean` or set PATS_EGRESS_POOL", pool, subnetTries, last)
}

// subnetTaken recognises the daemon saying the range is in use.
func subnetTaken(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "overlap") || strings.Contains(l, "already") || strings.Contains(l, "in use")
}

// defaultNetwork is the driver's network with a way out.
func (c *container) defaultNetwork() string {
	if c.driver == "podman" {
		return "podman"
	}
	return "bridge"
}

// waitReady reads the proxy's stderr until its listening line, then keeps
// draining it so the follower never blocks on a full pipe.
func waitReady(r io.Reader, timeout time.Duration) error {
	br := bufio.NewReader(r)
	ready := make(chan error, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if strings.HasPrefix(line, "listening ") {
				ready <- nil
				_, _ = io.Copy(io.Discard, br)
				return
			}
			if err != nil {
				ready <- fmt.Errorf("exited before listening: %s", strings.TrimSpace(line))
				return
			}
		}
	}()
	select {
	case err := <-ready:
		return err
	case <-time.After(timeout):
		return errors.New("not listening after " + timeout.String())
	}
}

// canaryScript probes every way out of the sandbox with the proxy env unset:
// tcp4, tcp6, a dns query over udp, and a name lookup. a leak prints leak=1.
// bash's /dev/tcp and /dev/udp need no tool in the image beyond bash itself;
// for udp only an answer counts, since connect and write cannot fail on it.
const canaryScript = `for c in timeout getent; do command -v $c >/dev/null 2>&1 || { echo "egress-canary missing=$c"; exit 0; }; done
leak=0
timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && leak=1
timeout 3 bash -c 'exec 3<>/dev/tcp/2606:4700:4700::1111/443' 2>/dev/null && leak=1
timeout 3 bash -c 'exec 3<>/dev/udp/8.8.8.8/53; printf "\000\001\001\000\000\001\000\000\000\000\000\000\007example\003com\000\000\001\000\001" >&3; read -t 2 -N 1 <&3' 2>/dev/null && leak=1
getent hosts example.com >/dev/null 2>&1 && leak=1
echo "egress-canary leak=$leak"`

// egressCanary runs the canary on the internal network before the proxy
// exists: with no route out every probe must fail. a hit refuses the run. an
// image without bash cannot run it, which is reported, not treated as a leak.
func (c *container) egressCanary(ctx context.Context, net string) error {
	args := []string{"run", "--rm", "--network", net}
	if c.driver == "docker" {
		args = append(args, "--dns", "0.0.0.0")
	}
	args = append(args, "--entrypoint", "bash", c.image, "-c", canaryScript)
	out, err := c.docker(ctx, args...)
	switch {
	case strings.Contains(out, "egress-canary leak=0"):
		return nil
	case strings.Contains(out, "egress-canary missing="):
		return fmt.Errorf("egress self-test could not run in image %s: %s", c.image, strings.TrimPrefix(out[strings.Index(out, "egress-canary missing="):], "egress-canary "))
	case strings.Contains(out, "egress-canary leak=1"):
		return fmt.Errorf("egress is not fail-closed under driver %s: a direct connection from the sandbox succeeded", c.driver)
	case err != nil:
		return fmt.Errorf("egress self-test could not run in image %s (needs bash): %w (%s)", c.image, err, out)
	}
	return fmt.Errorf("egress self-test gave no verdict: %s", out)
}

// dockerOut runs docker with stdout captured alone (no stderr mixed in) -- for
// reading file content out of an image. on failure the daemon's message lands
// on ExitError.Stderr (Output captures it there), so surface it -- else a
// failed `docker run` collapses to a bare "exit status 125".
func dockerOut(ctx context.Context, bin string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	var e *exec.ExitError
	if errors.As(err, &e) && len(e.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(e.Stderr)))
	}
	return out, err
}

func docker(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ProxyEnvOptions turns the sidecar's env into proxy.Options: the policy
// yaml, the inline CA, the roots path and the refused range. it is the one
// place the env protocol is read, shared by the sidecar entrypoint and any
// in-process use.
func ProxyEnvOptions(getenv func(string) string) (proxy.Options, error) {
	var o proxy.Options
	if y := getenv("PATS_EGRESS_POLICY"); y != "" {
		p, err := proxy.ParsePolicy([]byte(y))
		if err != nil {
			return o, fmt.Errorf("PATS_EGRESS_POLICY: %w", err)
		}
		o.Policy = p
	}
	o.CACert, o.CAKey = getenv("PATS_EGRESS_CA_CERT"), getenv("PATS_EGRESS_CA_KEY")
	if roots := getenv("PATS_EGRESS_ROOTS"); roots != "" {
		if strings.Contains(roots, "-----BEGIN") {
			o.Roots = []byte(roots)
		} else {
			b, err := os.ReadFile(roots)
			if err != nil {
				return o, fmt.Errorf("PATS_EGRESS_ROOTS: %w", err)
			}
			o.Roots = b
		}
	}
	if v := getenv("PATS_EGRESS_REFUSE"); v != "" {
		for _, s := range strings.Split(v, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(s))
			if err != nil {
				return o, fmt.Errorf("PATS_EGRESS_REFUSE: %w", err)
			}
			o.Refuse = append(o.Refuse, p)
		}
	}
	o.Listen = getenv("PATS_EGRESS_LISTEN")
	return o, nil
}
