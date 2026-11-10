package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
)

// namePrefix marks everything this package creates on the docker side
// (networks, sidecar containers) and the in-container CA mount path, so a
// crashed run is easy to spot and sweep up.
const namePrefix = "sbx-"

// runLabels are the labels pats puts on every container and network it
// makes: the run's token and owner (host, uid and client id), which `pats
// clean` finds a run's leftovers and judges them by. labels never change.
func (p *process) runLabels() []string {
	return []string{"--label", "pats.run=" + p.token, "--label", "pats.owner=" + p.owner()}
}

// hostOwner is the host, uid and client id that made a resource.
func hostOwner() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getuid(), clientID())
}

// clientID is a random id made once per user and kept beside the temp root:
// host and uid alone collide across machines sharing a daemon, and clean
// would then judge another machine's live run by an owner file it cannot
// see.
var clientID = sync.OnceValue(func() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	fresh := hex.EncodeToString(b)
	dir, err := CacheDir()
	if err != nil {
		// unpersisted: unique to this process, so never another's; a later
		// clean sees what it made as foreign and leaves it to --all.
		return fresh
	}
	// a pats before CacheDir kept its id in the legacy dir: carried over, so
	// runs an older binary started stay this client's.
	if id, err := loadClientID(filepath.Join(dir, "client-id"), func() string {
		if legacy := legacyCacheDir(); legacy != "" {
			if id := readClientID(filepath.Join(legacy, "client-id")); id != "" {
				return id
			}
		}
		return fresh
	}); err == nil {
		return id
	}
	return fresh
})

// loadClientID returns the id stored at p, publishing fresh() there first
// when there is none (fresh runs under p's lock, so a legacy id it reads is
// read under both locks, p's first): written to a temp file and hard-linked into place, so the
// file is whole whenever it exists. a malformed file (from before this) is
// replaced. a lock beside it serialises the whole read-repair-publish, so
// concurrent first runs, and concurrent repairs, all end on one id.
func loadClientID(p string, fresh func() string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	read := func() (string, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		if id := strings.TrimSpace(string(b)); clientIDRe.MatchString(id) {
			return id, nil
		}
		return "", errMalformedID
	}
	id, err := read()
	if err == nil {
		return id, nil
	}
	if errors.Is(err, errMalformedID) {
		if err := os.Remove(p); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "client-id-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(fresh() + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), p); err != nil && !os.IsExist(err) {
		return "", err
	}
	return read()
}

var errMalformedID = errors.New("malformed client id")

var clientIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

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
//
// env is what every exec in the session gets under its own: the machine's
// proxy, in an open sandbox behind one.
func (c *container) setupEgress(ctx context.Context, spec Spec) (netArgs []string, env map[string]string, teardown func(), err error) {
	switch {
	case spec.Egress.None:
		return []string{"--network", "none"}, nil, nil, nil
	case spec.Egress.Set:
		netArgs, teardown, err := c.startEgressProxy(ctx, spec)
		return netArgs, nil, teardown, err
	default:
		return c.openBehindProxy(ctx, spec.Egress.SelfTest)
	}
}

// openBehindProxy is an open sandbox's half of chaining: the machine's proxy
// env, and with SSL_CERT_FILE the merged bundle mounted and the trust env.
// under the self-test the upstream must answer from the driver's network,
// since there is no direct route to find. nothing without a machine proxy.
func (c *container) openBehindProxy(ctx context.Context, selfTest bool) ([]string, map[string]string, func(), error) {
	mp := c.o.Machine
	if mp.chain() == nil {
		return nil, nil, nil, nil
	}
	if selfTest {
		for _, u := range mp.Chain.Upstreams() {
			if err := c.upstreamReachable(ctx, u.Hostname(), u.Port()); err != nil {
				return nil, nil, nil, fmt.Errorf("egress self-test: the upstream %s: %w", proxy.Redact(u), err)
			}
		}
	}
	env := mp.Chain.AgentEnv()
	// the container starts with every proxy name set empty, so one the
	// engine would add (docker's config.json proxies) is not left beside
	// the machine's; the machine's values, credential and all, go on each
	// exec's stdin.
	var args []string
	for _, k := range proxyVars {
		args = append(args, "-e", k+"=")
	}
	if mp.trust() == nil {
		return args, env, nil, nil
	}
	m, err := c.p.prepare(Egress{}, mp.trust(), c.readBundle(ctx))
	if err != nil {
		return nil, nil, nil, err
	}
	return append(args, m.trustMounts()...), mergeEnv(env, m.trustEnv()), m.cleanup, nil
}

// readBundle reads the image's trust bundle in a throwaway container, once
// per image for the process: every pair of a sandbox asks for the same one.
func (c *container) readBundle(ctx context.Context) func() ([]byte, error) {
	return func() ([]byte, error) {
		return c.p.bundles.get(c.driver+"\x00"+c.image, func() ([]byte, error) {
			args := append(append([]string{"run", "--rm"}, c.p.runLabels()...), "--entrypoint", "cat", c.image, caBundlePath)
			return c.dockerOut(ctx, args...)
		})
	}
}

// bundleCache keeps what was read of each key. a read that failed is not
// kept: the next asker reads again.
type bundleCache struct {
	mu sync.Mutex
	m  map[string]*bundleEntry
}

type bundleEntry struct {
	mu   sync.Mutex
	data []byte
	ok   bool
}

func (b *bundleCache) get(key string, read func() ([]byte, error)) ([]byte, error) {
	b.mu.Lock()
	if b.m == nil {
		b.m = map[string]*bundleEntry{}
	}
	e := b.m[key]
	if e == nil {
		e = &bundleEntry{}
		b.m[key] = e
	}
	b.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ok {
		data, err := read()
		if err != nil {
			return nil, err
		}
		e.data, e.ok = data, true
	}
	return append([]byte(nil), e.data...), nil
}

// material is the driver-neutral half of a proxy-mode setup: what the proxy
// is told and what the sandboxed process is told, before any driver decides
// how the two meet. CA key material is held in memory only; the cert and the
// merged trust bundle go to caDir for the driver to mount.
type material struct {
	policyYAML string
	caCert     string // pem, "" without mitm
	caKey      string // pem, never written to disk
	caDir      string // "" without a bundle
	bundlePath string // merged image roots + run CA + machine trust, under caDir
	certPath   string // the certificates pats adds (run CA, machine trust), under caDir
}

// prepare renders the policy and, when it terminates tls anywhere, mints the
// run CA. with a CA or trust (the machine's SSL_CERT_FILE, behind an
// inspecting upstream) it merges both into the image's trust bundle
// (readBundle fetches that bundle from the driver). the caller removes
// caDir.
func (p *process) prepare(eg Egress, trust []byte, readBundle func() ([]byte, error)) (_ *material, err error) {
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
	if !eg.Policy.NeedsCA() && trust == nil {
		return m, nil
	}
	var added []byte
	if eg.Policy.NeedsCA() {
		certPEM, keyPEM, err := GenCA()
		if err != nil {
			return nil, fmt.Errorf("egress: gen mitm ca: %w", err)
		}
		m.caCert, m.caKey = string(certPEM), string(keyPEM)
		added = append(added, certPEM...)
	}
	if trust != nil {
		added = append(append(added, '\n'), trust...)
	}
	if m.caDir, err = p.mkTemp(namePrefix + "mitm-ca-"); err != nil {
		return nil, fmt.Errorf("egress: ca dir: %w", err)
	}
	m.certPath = filepath.Join(m.caDir, "ca.pem")
	if err := os.WriteFile(m.certPath, added, 0o644); err != nil {
		return nil, err
	}
	// merged bundle: the image's system roots + what pats adds. tunneled
	// (non-mitm) hosts still present real certs, so the system roots must
	// stay in the file.
	bundle, err := readBundle()
	if err != nil {
		return nil, fmt.Errorf("egress: read image trust bundle %s: %w", caBundlePath, err)
	}
	m.bundlePath = filepath.Join(m.caDir, "bundle.pem")
	if err := os.WriteFile(m.bundlePath, append(append(bundle, '\n'), added...), 0o644); err != nil {
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
	}
	if m.bundlePath != "" {
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
//
// NO_PROXY and ALL_PROXY are set empty: an engine that adds the host's proxy
// env (docker's config.json proxies) adds only what a container lacks, and
// neither may route around pats's proxy.
func (m *material) agentEnv(proxyURL string) map[string]string {
	env := map[string]string{
		"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL,
		"http_proxy": proxyURL, "https_proxy": proxyURL,
		"NO_PROXY": "", "no_proxy": "", "ALL_PROXY": "", "all_proxy": "",
	}
	return mergeEnv(env, m.trustEnv())
}

// trustEnv points every tls stack at the merged bundle; nil without one.
func (m *material) trustEnv() map[string]string {
	if m.bundlePath == "" {
		return nil
	}
	return map[string]string{
		"SSL_CERT_FILE":       agentBundlePath,
		"CURL_CA_BUNDLE":      agentBundlePath,
		"GIT_SSL_CAINFO":      agentBundlePath,
		"REQUESTS_CA_BUNDLE":  agentBundlePath,
		"CARGO_HTTP_CAINFO":   agentBundlePath,
		"NODE_EXTRA_CA_CERTS": agentCertPath,
	}
}

// trustMounts binds the merged bundle and the added certificates where
// trustEnv points; nil without a bundle.
func (m *material) trustMounts() []string {
	if m.bundlePath == "" {
		return nil
	}
	return []string{"-v", m.bundlePath + ":" + agentBundlePath + ":ro", "-v", m.certPath + ":" + agentCertPath + ":ro"}
}

// proxyVars are the proxy env names a tool may read, all of them.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy"}

// EgressPoolEnv overrides the egress pool (Options.EgressPool).
const EgressPoolEnv = "PATS_EGRESS_POOL"

// parsePool reads the range pats carves sandbox networks from, one /29 per
// pair (gateway, proxy, agent). docker's own pool is a handful of /16s and
// runs out under -j. an override is for the host whose lan is in the
// default; a bad one fails every proxy run rather than silently using the
// default.
func parsePool(v string) (netip.Prefix, error) {
	if v == "" {
		return netip.MustParsePrefix("10.213.0.0/16"), nil
	}
	p, err := netip.ParsePrefix(v)
	if err != nil || !p.Addr().Is4() || p.Bits() > 24 {
		return netip.Prefix{}, fmt.Errorf("egress pool %q (%s): want an ipv4 cidr of /24 or wider", v, EgressPoolEnv)
	}
	return p.Masked(), nil
}

const slotBits = 29

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

	pool, err := parsePool(c.egressPool)
	if err != nil {
		return nil, nil, err
	}
	subnet, err := c.createNetwork(ctx, net, pool)
	if err != nil {
		return nil, nil, err
	}

	// the audit log is streamed live by a `docker logs -f` follower (started
	// below), not dumped at the end. teardown stops it + removes proxy + network
	// + the mitm CA dir, using a fresh context so it still runs if the agent run
	// was cancelled.
	var logs *follower
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

		if logs != nil {
			logs.stop(c.p.clock, c.p.t.followerGrace)
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

	m, err = c.p.prepare(spec.Egress, c.o.Machine.trust(), c.readBundle(ctx))
	if err != nil {
		teardown()
		return nil, nil, err
	}

	// the proxy sits at a fixed address on the internal net (the gateway takes
	// the first), so the agent needs no resolver to find it.
	proxyIP := subnet.Addr().Next().Next()
	runArgs := []string{"run", "-d", "--rm", "--name", proxyName, "--network", net, "--ip", proxyIP.String()}
	runArgs = append(runArgs, c.p.runLabels()...)
	runArgs = append(runArgs,
		// the sidecar also sits on the default network, where a container
		// could hand-craft frames at it to be routed out: it forwards nothing
		// and needs no capability to listen on an unprivileged port.
		"--sysctl", "net.ipv4.ip_forward=0", "--sysctl", "net.ipv6.conf.all.forwarding=0",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
	)
	runArgs = append(runArgs, engines[c.driver].runFlags...)
	// the machine's proxy reaches the sidecar under pats's keys alone: every
	// name a tool reads is set empty, so one the engine would add (docker's
	// config.json proxies) changes nothing.
	for _, k := range proxyVars {
		runArgs = append(runArgs, "-e", k+"=")
	}
	// the proxy sidecar is rootless and reads its config from env; the trust
	// bundle is the one thing too big for env, so it is mounted read-only.
	if m.bundlePath != "" {
		runArgs = append(runArgs, "-v", m.bundlePath+":"+agentBundlePath+":ro")
	}
	penv := mergeEnv(m.proxyEnv(agentBundlePath), c.o.Machine.chain().Env())
	penv["PATS_EGRESS_REFUSE"] = pool.String()
	// on the internal address only: on :8080 any container on the default
	// network could use this pair's policy and write into its audit.
	penv["PATS_EGRESS_LISTEN"] = proxyIP.String() + ":" + proxyPort
	// the policy and the ca key ride in the cli's environment, never on its
	// command line.
	flags, r := envFlags(c.r, "-e", penv)
	runArgs = append(runArgs, flags...)
	runArgs = append(runArgs, img)
	if out, err := combined(ctx, r, runArgs...); err != nil {
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
	if logs, err = follow(c.r, nil, audit, "logs", "-f", proxyName); err != nil {
		teardown()
		return nil, nil, fmt.Errorf("egress: follow proxy logs: %w", err)
	}
	if err := logs.ready(c.p.clock, c.p.t.proxyReady); err != nil {
		teardown()
		if up := c.o.Machine.chain(); up != nil {
			return nil, nil, fmt.Errorf("egress: proxy %s, on the %s network, behind the upstream %s: %w",
				proxyName, c.defaultNetwork(), up.Describe(), err)
		}
		return nil, nil, fmt.Errorf("egress: proxy %s: %w", proxyName, err)
	}
	if spec.Egress.SelfTest {
		if err := c.proxyReachable(ctx, net, proxyIP.String()+":"+proxyPort); err != nil {
			teardown()
			return nil, nil, err
		}
	}

	netArgs := append([]string{"--network", net}, engines[c.driver].agentNet...)
	netArgs = append(netArgs, m.trustMounts()...)
	for _, kv := range sortedEnv(m.agentEnv("http://" + proxyIP.String() + ":" + proxyPort)) {
		netArgs = append(netArgs, "-e", kv)
	}
	return netArgs, teardown, nil
}

// createNetwork makes the pair's internal network on the next free /29 of the
// pool, moving on when the daemon already has that range (another pats, or a
// network a crashed run left behind).
func (c *container) createNetwork(ctx context.Context, name string, pool netip.Prefix) (netip.Prefix, error) {
	var last string
	for range subnetTries {
		subnet := c.p.nextSubnet(pool)
		args := append([]string{"network", "create", "--internal"}, c.p.runLabels()...)
		args = append(args, engines[c.driver].internalNet...)
		args = append(args, "--subnet", subnet.String())
		out, err := c.docker(ctx, append(args, name)...)
		if err == nil {
			return subnet, nil
		}
		last = out
		if nameTaken(out) {
			return netip.Prefix{}, fmt.Errorf("egress: create network: the name %s is taken (%s); `pats clean` removes a dead run's", name, out)
		}
		if !subnetTaken(out) {
			// a create the cli saw fail may still have made it; the name is
			// this pair's alone.
			rctx, cancel := context.WithTimeout(context.Background(), c.p.t.cliCleanup)
			_, _ = c.docker(rctx, "network", "rm", name)
			cancel()
			return netip.Prefix{}, fmt.Errorf("egress: create network: %w (%s)", err, out)
		}
	}
	return netip.Prefix{}, fmt.Errorf("egress: no free subnet in %s after %d tries (%s); `pats clean` or set %s", pool, subnetTries, last, EgressPoolEnv)
}

// nameTaken recognises the daemon saying a network of that name exists:
// docker's "network with name x already exists", podman's "network name x
// already used".
func nameTaken(out string) bool {
	l := strings.ToLower(out)
	return (strings.Contains(l, "network with name ") && strings.Contains(l, "already exists")) ||
		(strings.Contains(l, "network name ") && strings.Contains(l, "already used"))
}

// subnetTaken recognises the daemon saying the range is in use: docker's
// "pool overlaps", podman's "subnet x is already used".
func subnetTaken(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "overlap") || strings.Contains(l, "already") || strings.Contains(l, "in use")
}

// defaultNetwork is the driver's network with a way out.
func (c *container) defaultNetwork() string { return engines[c.driver].defaultNetwork }

// waitReady reads the proxy's stderr until its listening line, then keeps
// draining it so the follower never blocks on a full pipe.
func waitReady(r io.Reader, c clock, timeout time.Duration) error {
	br := bufio.NewReader(r)
	ready := make(chan error, 1)
	go func() {
		last := ""
		for {
			line, err := br.ReadString('\n')
			if strings.HasPrefix(line, "listening ") {
				ready <- nil
				_, _ = io.Copy(io.Discard, br)
				return
			}
			if t := strings.TrimSpace(line); t != "" {
				last = t
			}
			if err != nil {
				ready <- fmt.Errorf("exited before listening: %s", last)
				return
			}
		}
	}()
	select {
	case err := <-ready:
		return err
	case <-c.After(timeout):
		return errors.New("not listening after " + timeout.String())
	}
}

// proxyReachable is the canary's other half: from the internal network, with
// the proxy up, its port must answer. a host firewall that drops traffic on
// the driver's internal networks would otherwise fail every pair with a
// connection error the agent reports in its own words.
func (c *container) proxyReachable(ctx context.Context, net, addr string) error {
	host, port, _ := strings.Cut(addr, ":")
	out, err := c.docker(ctx, c.probeArgs(net, probeReach, host, port)...)
	reached, said := reachVerdict(out)
	switch {
	case reached:
		return nil
	case said:
		return fmt.Errorf("egress self-test: the sandbox cannot reach the proxy at %s under driver %s; something on the host drops traffic on its internal networks (a firewall rule, a FORWARD policy)", addr, c.driver)
	case err != nil:
		return fmt.Errorf("egress self-test could not run in image %s: %w (%s)", c.image, err, out)
	}
	return fmt.Errorf("egress self-test gave no verdict: %s", out)
}

// upstreamReachable runs the reach probe at host:port from a throwaway
// container on the driver's default network, where an open sandbox runs.
func (c *container) upstreamReachable(ctx context.Context, host, port string) error {
	run := append(append([]string{"run", "--rm"}, c.p.runLabels()...), engines[c.driver].runFlags...)
	argv := probeReach.argv(host, port)
	run = append(append(run, "--network", c.defaultNetwork(), "--entrypoint", argv[0], c.image), argv[1:]...)
	out, err := c.docker(ctx, run...)
	reached, said := reachVerdict(out)
	switch {
	case reached:
		return nil
	case said:
		return fmt.Errorf("unreachable from the %s network under driver %s", c.defaultNetwork(), c.driver)
	case err != nil:
		return fmt.Errorf("the probe could not run in image %s: %w (%s)", c.image, err, out)
	}
	return fmt.Errorf("the probe gave no verdict: %s", out)
}

// probeArgs runs p in a throwaway container of the sandbox image on net.
func (c *container) probeArgs(net string, p probe, args ...string) []string {
	run := append(append([]string{"run", "--rm"}, c.p.runLabels()...), "--network", net)
	run = append(run, engines[c.driver].agentNet...)
	argv := p.argv(args...)
	return append(append(run, "--entrypoint", argv[0], c.image), argv[1:]...)
}

// egressCanary runs the canary on the internal network before the proxy
// exists: with no route out every probe must fail. a hit refuses the run. an
// image without bash cannot run it, which is reported, not treated as a leak.
func (c *container) egressCanary(ctx context.Context, net string) error {
	out, err := c.docker(ctx, c.probeArgs(net, probeCanary)...)
	if v := canaryVerdict(out, c.driver, c.image); v == nil || err == nil || strings.Contains(out, "egress-canary") {
		return v
	}
	return fmt.Errorf("egress self-test could not run in image %s (needs bash): %w (%s)", c.image, err, out)
}

// ProxyEnvOptions turns the sidecar's env into proxy.Options: the policy
// yaml, the inline CA, the roots path, the refused range and the upstream
// (pats's keys, else the machine's proxy env). it is the one
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
	c, err := proxy.ChainFromEnv(getenv)
	if err != nil {
		return o, err
	}
	o.Chain = c
	return o, nil
}
