package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
		if err := canaryVerdict(out.String(), "lxd", l.o.Image); err != nil {
			return err
		}
	}

	m, err := prepare(spec.Egress, func() ([]byte, error) {
		var out, errb bytes.Buffer
		code, err := s.exec(ctx, Cmd{Argv: []string{"cat", caBundlePath}, Root: true, Dir: "/"}, &out, &errb)
		if err == nil && code != 0 {
			err = fmt.Errorf("%s", strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), err
	})
	if err != nil {
		return err
	}
	e.m = m

	img, err := l.proxyImage(ctx)
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
		return fmt.Errorf("egress proxy: %w", err)
	}
	if err := s.setAddress(ctx, e.proxy, netip.PrefixFrom(e.ip, e.prefix)); err != nil {
		return err
	}
	// the proxy routes nothing: a root agent could aim frames at it.
	var out bytes.Buffer
	if code, err := p.exec(ctx, Cmd{Argv: []string{"sh", "-c", "sysctl -qw net.ipv4.ip_forward=0 net.ipv6.conf.all.forwarding=0"}, Root: true, Dir: "/"}, &out, &out); err != nil || code != 0 {
		return fmt.Errorf("egress proxy forwarding off: %v %s", err, strings.TrimSpace(out.String()))
	}
	if m.caCert != "" {
		if err := p.Push(ctx, m.bundlePath, lxdProxyRoots); err != nil {
			return err
		}
		if err := s.Push(ctx, m.bundlePath, agentBundlePath); err != nil {
			return err
		}
		if err := s.Push(ctx, m.certPath, agentCertPath); err != nil {
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
	if e.m.caCert != "" {
		roots = lxdProxyRoots
	}
	penv := e.m.proxyEnv(roots)
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
	args := p.execArgs(Cmd{Argv: []string{"/usr/local/bin/pats", "egress", "run"}, Env: penv, Dir: "/"})
	run, err := follow(s.l.r, audit, args...)
	if err != nil {
		return fmt.Errorf("egress: start proxy: %w", err)
	}
	e.run = run
	if err := run.ready(s.l.p.clock, s.l.p.t.proxyReady); err != nil {
		return fmt.Errorf("egress: proxy %s: %w", e.proxy, err)
	}
	return nil
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
func (s *lxdSession) closeEgress() {
	e := s.egress
	if e == nil {
		return
	}
	if e.run != nil {
		e.run.stop(s.l.p.clock, 0)
	}
	if e.auditF != nil {
		_ = e.auditF.Close()
	}
	_ = s.l.deleteInstance(e.proxy)
	if e.m != nil {
		e.m.cleanup()
	}
}

// proxyImage is the proxy container image on l's server, made once per
// process (every proxied pair needs it, and resolving it means fetching the
// binary); see buildProxyImage.
func (l *lxd) proxyImage(ctx context.Context) (string, error) {
	// held across the build, so concurrent pairs wait for one build.
	x := &l.p.lxd
	x.proxyMu.Lock()
	defer x.proxyMu.Unlock()
	if alias, ok := x.proxyImages[l.o.Remote]; ok {
		return alias, nil
	}
	alias, err := l.buildProxyImage(ctx)
	if err == nil {
		x.proxyImages[l.o.Remote] = alias
	}
	return alias, err
}

// buildProxyImage provisions the proxy container image: a minimal ubuntu with
// the pats binary for the server's arch, an unprivileged user to run it, and
// dhcp on the uplink nic. keyed like any provisioned image, the binary's bytes
// included, so a new pats makes a new one.
func (l *lxd) buildProxyImage(ctx context.Context) (string, error) {
	arch, err := l.serverArch(ctx)
	if err != nil {
		return "", err
	}
	material, err := MkTemp("pats-proxy-material-")
	if err != nil {
		return "", err
	}
	defer RemoveTemp(material)
	if err := proxyBinary(ctx, l.p.reg, l.o.ProxyBin, arch, filepath.Join(material, "pats"), l.p.t.proxyFetch); err != nil {
		return "", err
	}
	netplan := "network:\n  version: 2\n  ethernets:\n    uplink0:\n      dhcp4: true\n"
	if err := os.WriteFile(filepath.Join(material, "60-pats-uplink.yaml"), []byte(netplan), 0o600); err != nil {
		return "", err
	}
	script := `set -e
cp ` + ProvisionMount + `/pats /usr/local/bin/pats && chmod 0755 /usr/local/bin/pats
id -u ` + lxdProxyUser + ` >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin ` + lxdProxyUser + `
mkdir -p /etc/netplan /etc/pats && cp ` + ProvisionMount + `/60-pats-uplink.yaml /etc/netplan/ && chmod 0600 /etc/netplan/60-pats-uplink.yaml`
	if err := os.WriteFile(filepath.Join(material, "script"), []byte(script), 0o644); err != nil {
		return "", err
	}
	pl := newLXD(l.p, l.r, Options{Driver: "lxd", Remote: l.o.Remote, Uplink: l.o.Uplink})
	spec := LXDImageSpec{Base: lxdProxyBase, Material: material, Argv: []string{"/bin/sh", ProvisionMount + "/script"},
		Progress: prefixed(l.o.Progress, "egress proxy: "), Warn: prefixed(l.o.Warn, "egress proxy: ")}
	var log bytes.Buffer
	alias, _, err := pl.image(ctx, spec, &log)
	if err != nil {
		return "", fmt.Errorf("%w%s", err, LogTail(log.String()))
	}
	return alias, nil
}

func prefixed(f func(string), p string) func(string) {
	if f == nil {
		return nil
	}
	return func(msg string) { f(p + msg) }
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
