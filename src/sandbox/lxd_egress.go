package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// the proxy topology under lxd is fail-closed by construction, not by acl:
// the pair's bridge has no host address (so no dhcp, no dns, no routing, no
// host to reach), and only the agent and its proxy are on it, at static
// addresses out of the egress pool. the proxy's other nic, on the shared
// uplink, is the only way out, and the proxy forwards nothing. a bridge acl
// with both defaults reject still fences the bridge-host boundary, in case
// something else on the server routes. its one rule lets the agent reach the
// proxy port: with br_netfilter loaded (docker loads it) traffic between two
// nics on one bridge passes the host's forward hook, where the acl sits.

// LXDProxyBinEnv names a linux pats binary for the server's arch, for a dev
// build with no published image.
const LXDProxyBinEnv = "PATS_LXD_PROXY_BIN"

// lxdProxyBase is what the proxy container is provisioned on.
const lxdProxyBase = "ubuntu-minimal:26.04"

// lxdProxyUser runs `pats egress run` in the proxy container.
const lxdProxyUser = "patsproxy"

// in-instance paths of what the proxy setup pushes.
const (
	lxdProxyRoots = "/etc/pats/roots.pem"
	lxdCADir      = "/sbx-ca"
)

// lxdEgress is a pair's proxy: its container, the process serving the policy
// in it, and the material both sides were given.
type lxdEgress struct {
	proxy   string // instance name
	ip      netip.Addr
	agentIP netip.Addr
	prefix  int
	refuse  netip.Prefix // the egress pool, which the proxy refuses to dial
	m       *material
	run     *follower // `pats egress run`, through lxc exec
	auditF  *os.File
}

// startEgress builds the topology around the pair's (stopped) agent: the
// bridge and acl, the agent's nic. the proxy itself comes up once the agent
// is running and has passed the canary.
func (s *lxdSession) startEgressNetwork(ctx context.Context) error {
	l := s.l
	s.bridge = l.bridgeName(s.name)
	aclArgs := append([]string{"network", "acl", "create", l.ref(s.bridge), "--project", "default"}, l.meta()...)
	if _, err := l.lxc(ctx, aclArgs...); err != nil {
		return err
	}
	netArgs := append([]string{"network", "create", l.ref(s.bridge), "--project", "default",
		"ipv4.address=none", "ipv6.address=none", "security.acls=" + s.bridge,
		"security.acls.default.egress.action=reject", "security.acls.default.ingress.action=reject"}, l.meta()...)
	if _, err := l.lxc(ctx, netArgs...); err != nil {
		return err
	}
	_, err := l.lxc(ctx, "config", "device", "add", s.inst(), "pats0", "nic", "--project", lxdProject,
		"network="+s.bridge, "name=pats0")
	return err
}

// setAddress puts a static address on pats0 inside an instance.
func (s *lxdSession) setAddress(ctx context.Context, inst string, addr netip.Prefix) error {
	script := `ip link set pats0 up && ip addr replace ` + addr.String() + ` dev pats0`
	var out bytes.Buffer
	t := &lxdSession{l: s.l, name: inst}
	code, err := t.exec(ctx, Cmd{Argv: []string{"sh", "-c", script}, Root: true, Dir: "/"}, &out, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("address %s on %s: %w", addr, inst, err)
	}
	return nil
}

// startProxy runs the canary from the agent, brings the proxy container up
// next to it, starts `pats egress run` there with the pair's policy, and
// gives the agent the proxy env.
func (s *lxdSession) startProxy(ctx context.Context, spec Spec) error {
	l := s.l
	pool, err := parsePool(l.o.EgressPool)
	if err != nil {
		return err
	}
	subnet := l.p.nextSubnet(pool)
	e := &lxdEgress{proxy: s.name + "-p", ip: subnet.Addr().Next().Next(), agentIP: subnet.Addr().Next().Next().Next(),
		prefix: subnet.Bits(), refuse: pool}
	s.egress = e
	if _, err := l.lxc(ctx, "network", "acl", "rule", "add", l.ref(s.bridge), "egress", "--project", "default",
		"action=allow", "source="+e.agentIP.String()+"/32", "destination="+e.ip.String()+"/32",
		"protocol=tcp", "destination_port="+proxyPort); err != nil {
		return err
	}
	if err := s.setAddress(ctx, s.name, netip.PrefixFrom(e.agentIP, e.prefix)); err != nil {
		return err
	}
	if spec.Egress.SelfTest {
		var out bytes.Buffer
		_, _ = s.exec(ctx, Cmd{Argv: probeCanary.argv(), Root: true, Dir: "/"}, &out, &out)
		if err := canaryVerdict(out.String(), "lxd", l.s.Image); err != nil {
			return err
		}
	}

	m, err := l.p.prepare(spec.Egress, l.o.Machine.trust(), s.readBundle(ctx))
	if err != nil {
		return err
	}
	e.m = m

	img, _, err := l.proxyImage(ctx, false, nil)
	if err != nil {
		return fmt.Errorf("egress proxy image: %w", err)
	}
	uplink, err := l.uplink(ctx)
	if err != nil {
		return err
	}
	p := &lxdSession{l: l, name: e.proxy}
	args := append([]string{"init", l.ref(img), p.inst(), "--project", lxdProject, "--storage", l.pool}, l.metaFlags()...)
	if _, err := l.lxc(ctx, args...); err != nil {
		return err
	}
	for _, d := range [][]string{{"pats0", s.bridge}, {"uplink0", uplink}} {
		if _, err := l.lxc(ctx, "config", "device", "add", p.inst(), d[0], "nic", "--project", lxdProject,
			"network="+d[1], "name="+d[0]); err != nil {
			return err
		}
	}
	if _, err := l.lxc(ctx, "start", p.inst(), "--project", lxdProject); err != nil {
		return err
	}
	if err := p.waitReady(ctx, true); err != nil {
		return fmt.Errorf("egress proxy: %w", noRouteHint(err, uplink))
	}
	if err := s.setAddress(ctx, e.proxy, netip.PrefixFrom(e.ip, e.prefix)); err != nil {
		return err
	}
	// the proxy routes nothing: a root agent could aim frames at it.
	var out bytes.Buffer
	if code, err := p.exec(ctx, Cmd{Argv: []string{"sh", "-c", "sysctl -qw net.ipv4.ip_forward=0 net.ipv6.conf.all.forwarding=0"}, Root: true, Dir: "/"}, &out, &out); err != nil || code != 0 {
		return fmt.Errorf("egress proxy forwarding off: %v %s", err, strings.TrimSpace(out.String()))
	}
	if m.bundlePath != "" {
		if err := p.Push(ctx, m.bundlePath, lxdProxyRoots); err != nil {
			return err
		}
		if err := s.pushTrust(ctx, m); err != nil {
			return err
		}
	}
	if err := s.runProxy(ctx, p, spec.Egress.AuditPath); err != nil {
		return err
	}
	addr := e.ip.String() + ":" + proxyPort
	if spec.Egress.SelfTest {
		var out bytes.Buffer
		_, _ = s.exec(ctx, Cmd{Argv: probeReach.argv(e.ip.String(), proxyPort), Root: true, Dir: "/"}, &out, &out)
		if reached, _ := reachVerdict(out.String()); !reached {
			return fmt.Errorf("egress self-test: the sandbox cannot reach its proxy at %s on bridge %s. %s", addr, s.bridge, dockerBridgeHint)
		}
		if spec.Sudo {
			if err := s.hostileProbe(ctx); err != nil {
				return err
			}
		}
	}
	s.env = m.agentEnv("http://" + addr)
	return nil
}

// runProxy starts `pats egress run` in the proxy container as an
// unprivileged user, listening on the bridge address only, its stdout (the
// audit) streamed to auditPath, and waits for its listening line.
func (s *lxdSession) runProxy(ctx context.Context, p *lxdSession, auditPath string) error {
	e := s.egress
	roots := ""
	if e.m.bundlePath != "" {
		roots = lxdProxyRoots
	}
	penv := mergeEnv(e.m.proxyEnv(roots), s.l.o.Machine.chain().Env())
	penv["PATS_EGRESS_REFUSE"] = e.refuse.String()
	penv["PATS_EGRESS_LISTEN"] = e.ip.String() + ":" + proxyPort
	var uid bytes.Buffer
	if code, err := p.exec(ctx, Cmd{Argv: probeProxyUser.argv(), Root: true, Dir: "/"}, &uid, io.Discard); err != nil || code != 0 {
		return fmt.Errorf("egress proxy user: %v", err)
	}
	var err error
	if p.uid, err = uidVerdict(uid.String()); err != nil {
		return fmt.Errorf("egress proxy user: %w", err)
	}

	var audit io.Writer = io.Discard
	if auditPath != "" {
		f, err := os.Create(auditPath)
		if err != nil {
			return fmt.Errorf("egress: audit log: %w", err)
		}
		e.auditF, audit = f, f
	}
	args, in := p.execArgs(Cmd{Argv: []string{"/usr/local/bin/pats", "egress", "run"}, Env: penv, Dir: "/"}, nil)
	run, err := follow(s.l.r, in, audit, args...)
	if err != nil {
		return fmt.Errorf("egress: start proxy: %w", err)
	}
	e.run = run
	if err := run.ready(s.l.p.clock, s.l.p.t.proxyReady); err != nil {
		if up := s.l.o.Machine.chain(); up != nil {
			return fmt.Errorf("egress: proxy %s, on the uplink, behind the upstream %s: %w", e.proxy, up.Describe(), err)
		}
		return fmt.Errorf("egress: proxy %s: %w", e.proxy, err)
	}
	return nil
}

// readBundle reads the image's trust bundle out of the instance.
func (s *lxdSession) readBundle(ctx context.Context) func() ([]byte, error) {
	return func() ([]byte, error) {
		var out, errb bytes.Buffer
		code, err := s.exec(ctx, Cmd{Argv: []string{"cat", caBundlePath}, Root: true, Dir: "/"}, &out, &errb)
		if err == nil && code != 0 {
			err = fmt.Errorf("%s", strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), err
	}
}

// pushTrust puts the merged bundle and the added certificates where the
// trust env points.
func (s *lxdSession) pushTrust(ctx context.Context, m *material) error {
	if err := s.Push(ctx, m.bundlePath, agentBundlePath); err != nil {
		return err
	}
	return s.Push(ctx, m.certPath, agentCertPath)
}

// openBehindProxy hands an open sandbox the machine's proxy env and, with
// SSL_CERT_FILE, the merged bundle and the trust env pointing at it.
func (s *lxdSession) openBehindProxy(ctx context.Context) error {
	mp := s.l.o.Machine
	if mp.chain() == nil {
		return nil
	}
	env := mp.Chain.AgentEnv()
	if mp.trust() != nil {
		m, err := s.l.p.prepare(Egress{}, mp.trust(), s.readBundle(ctx))
		if err != nil {
			return err
		}
		defer m.cleanup()
		if err := s.pushTrust(ctx, m); err != nil {
			return err
		}
		env = mergeEnv(env, m.trustEnv())
	}
	s.machineEnv = env
	return nil
}

// openRejects are openRanges less the upstream's addresses: an open sandbox
// behind a proxy on the lan must reach it. a name is resolved here, on
// pats's host.
func (l *lxd) openRejects(ctx context.Context) ([]string, error) {
	c := l.o.Machine.chain()
	if c == nil {
		return openRanges, nil
	}
	var addrs []netip.Addr
	for _, u := range c.Upstreams() {
		if a, err := netip.ParseAddr(u.Hostname()); err == nil {
			addrs = append(addrs, a.Unmap())
			continue
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
		if err != nil {
			return nil, fmt.Errorf("lxd: resolve the upstream %s: %w", proxy.Redact(u), err)
		}
		for _, ip := range ips {
			addrs = append(addrs, ip.Unmap())
		}
	}
	return carve(openRanges, addrs), nil
}

// carve is ranges with each of addrs cut out, as the fewest prefixes.
func carve(ranges []string, addrs []netip.Addr) []string {
	var out []string
	for _, r := range ranges {
		ps := []netip.Prefix{netip.MustParsePrefix(r)}
		for _, a := range addrs {
			var next []netip.Prefix
			for _, p := range ps {
				next = append(next, cutAddr(p, a)...)
			}
			ps = next
		}
		for _, p := range ps {
			out = append(out, p.String())
		}
	}
	return out
}

// cutAddr is p less a: p's halves down to a's own, each but a's whole.
func cutAddr(p netip.Prefix, a netip.Addr) []netip.Prefix {
	if !p.Contains(a) {
		return []netip.Prefix{p}
	}
	if p.Bits() == a.BitLen() {
		return nil
	}
	lo := netip.PrefixFrom(p.Addr(), p.Bits()+1)
	b := p.Addr().AsSlice()
	b[p.Bits()/8] |= 0x80 >> (p.Bits() % 8)
	hiAddr, _ := netip.AddrFromSlice(b)
	hi := netip.PrefixFrom(hiAddr, p.Bits()+1)
	return append(cutAddr(lo, a), cutAddr(hi, a)...)
}

// hostileProbe is the self-test of a sandbox whose agent may sudo: as root,
// aim a default route at the bridge's host side through a static neighbour
// entry and try out. the bridge has no address and its acl rejects, so this
// must fail.
func (s *lxdSession) hostileProbe(ctx context.Context) error {
	out, err := s.l.lxc(ctx, "query", s.queryPath("/1.0/networks/"+s.bridge+"/state?project=default"))
	if err != nil {
		return err
	}
	var st struct {
		Hwaddr string `json:"hwaddr"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Hwaddr == "" {
		return fmt.Errorf("egress self-test: bridge %s mac: %v", s.bridge, err)
	}
	gw := s.egress.agentIP.Next()
	var res bytes.Buffer
	_, _ = s.exec(ctx, Cmd{Argv: probeHostile.argv(gw.String(), st.Hwaddr), Root: true, Dir: "/"}, &res, &res)
	return hostileVerdict(res.String())
}

// closeEgress stops the proxy process and container and drops the material.
func (s *lxdSession) closeEgress() error {
	e := s.egress
	if e == nil {
		return nil
	}
	// the proxy instance goes first: its exec stream then ends on its own,
	// and the audit lines still in flight reach the file, as for docker's
	// follower. killing the lxc client first could drop the denied-host tail.
	err := s.l.deleteInstance(e.proxy)
	if e.run != nil {
		e.run.stop(s.l.p.clock, s.l.p.t.followerGrace)
	}
	if e.auditF != nil {
		_ = e.auditF.Close()
	}
	if e.m != nil {
		e.m.cleanup()
	}
	return err
}

// proxyImage is the proxy container image on l's server, made once per
// process (every proxied pair needs it, and resolving it means fetching the
// binary) unless force; see buildProxyImage. a sandbox's pinned one is used
// as is. l is ready.
func (l *lxd) proxyImage(ctx context.Context, force bool, log io.Writer) (alias string, built bool, err error) {
	if l.s.ProxyImage != "" {
		return l.s.ProxyImage, false, nil
	}
	// held across the build, so concurrent pairs wait for one build.
	x := &l.p.lxd
	x.proxyMu.Lock()
	defer x.proxyMu.Unlock()
	if alias, ok := x.proxyImages[l.o.Remote]; ok && !force {
		return alias, false, nil
	}
	alias, built, err = l.buildProxyImage(ctx, force, log)
	if err == nil {
		x.proxyImages[l.o.Remote] = alias
	}
	return alias, built, err
}

// buildProxyImage provisions the proxy container image: a minimal ubuntu with
// the pats binary for the server's arch, an unprivileged user to run it, and
// dhcp on the uplink nic. keyed like any provisioned image, the binary's bytes
// included, so a new pats makes a new one.
func (l *lxd) buildProxyImage(ctx context.Context, force bool, log io.Writer) (string, bool, error) {
	arch, err := l.serverArch(ctx)
	if err != nil {
		return "", false, err
	}
	material, err := l.p.mkTemp("pats-proxy-material-")
	if err != nil {
		return "", false, err
	}
	defer RemoveTemp(material)
	if err := proxyBinary(ctx, l.p.reg, l.o.ProxyBin, arch, filepath.Join(material, "pats"), l.p.t.proxyFetch); err != nil {
		return "", false, err
	}
	netplan := "network:\n  version: 2\n  ethernets:\n    uplink0:\n      dhcp4: true\n"
	if err := os.WriteFile(filepath.Join(material, "60-pats-uplink.yaml"), []byte(netplan), 0o600); err != nil {
		return "", false, err
	}
	script := `set -e
cp ` + ProvisionMount + `/pats /usr/local/bin/pats && chmod 0755 /usr/local/bin/pats
id -u ` + lxdProxyUser + ` >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin ` + lxdProxyUser + `
mkdir -p /etc/netplan /etc/pats && cp ` + ProvisionMount + `/60-pats-uplink.yaml /etc/netplan/ && chmod 0600 /etc/netplan/60-pats-uplink.yaml`
	if err := os.WriteFile(filepath.Join(material, "script"), []byte(script), 0o644); err != nil {
		return "", false, err
	}
	// always a container, whatever the sandboxes are.
	pl := &lxd{p: l.p, r: l.r, o: Options{Driver: "lxd", Remote: l.o.Remote, Uplink: l.o.Uplink}, pool: l.pool}
	spec := lxdImageSpec{Base: lxdProxyBase, Material: material, Argv: []string{"/bin/sh", ProvisionMount + "/script"},
		Role: "proxy", Force: force}
	// a release's proxy is its version on this arch; a dev build's is its
	// binary's bytes, so every rebuild of pats makes a new one.
	if releaseBuild() {
		spec.Key = "proxy " + version.Info.Version + " " + arch
	}
	var buf bytes.Buffer
	if log == nil {
		log = io.Discard
	}
	alias, _, _, built, err := pl.image(ctx, spec, io.MultiWriter(&buf, log))
	if err != nil {
		return "", false, fmt.Errorf("%w%s", err, LogTail(buf.String()))
	}
	return alias, built, nil
}

// serverArch is the lxd server's architecture as a go arch.
func (l *lxd) serverArch(ctx context.Context) (string, error) {
	out, err := l.lxc(ctx, "query", l.o.Remote+":/1.0")
	if err != nil {
		return "", err
	}
	var srv struct {
		Environment struct {
			Architectures []string `json:"architectures"`
		} `json:"environment"`
	}
	if err := json.Unmarshal([]byte(out), &srv); err != nil || len(srv.Environment.Architectures) == 0 {
		return "", fmt.Errorf("lxd: server architecture: %v", err)
	}
	switch a := srv.Environment.Architectures[0]; a {
	case "x86_64":
		return "amd64", nil
	case "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("lxd: server architecture %s has no pats build", a)
	}
}

// proxyBinary puts a linux pats for arch at dst: src (Options.ProxyBin) when
// set, this binary when it is one, else the published proxy image's.
func proxyBinary(ctx context.Context, reg registry, src, arch, dst string, timeout time.Duration) error {
	if src == "" && runtime.GOOS == "linux" && runtime.GOARCH == arch {
		if exe, err := os.Executable(); err == nil {
			src = exe
		}
	}
	if src != "" {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o755)
	}
	img, _ := ProxyImage("")
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := reg.fetchImageFile(deadline, img, arch, "pats", dst); err != nil {
		return fmt.Errorf("fetch the proxy binary from %s (or set %s): %w", img, LXDProxyBinEnv, err)
	}
	return nil
}
