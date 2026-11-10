package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
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
// per image of an engine's endpoint for the process: every pair of a
// sandbox asks for the same one. a run starts from pinned image ids, so the
// name is the image.
func (c *container) readBundle(ctx context.Context) func() ([]byte, error) {
	return func() ([]byte, error) {
		key := strings.Join([]string{c.driver, c.o.ContainerHost, c.o.ContainerConnection, c.image}, "\x00")
		return c.p.bundles.get(ctx, key, func() ([]byte, error) {
			args := append(append([]string{"run", "--rm"}, c.p.runLabels()...), "--entrypoint", "cat", c.image, caBundlePath)
			return c.dockerOut(ctx, args...)
		})
	}
}

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
// already used" and, from netavark, "network already exists x".
func nameTaken(out string) bool {
	l := strings.ToLower(out)
	return (strings.Contains(l, "network") && strings.Contains(l, "already exists")) ||
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
