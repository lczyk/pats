package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// lxdProxyBinEnv names a linux pats binary for the server's arch, for a dev
// build with no published image.
const lxdProxyBinEnv = "PATS_LXD_PROXY_BIN"

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
	m       *material
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	auditF  *os.File
}

// startEgress builds the topology around the pair's (stopped) agent: the
// bridge and acl, the agent's nic. the proxy itself comes up once the agent
// is running and has passed the canary.
func (s *lxdSession) startEgressNetwork(ctx context.Context) error {
	l := s.l
	s.bridge = bridgeName(s.name)
	aclArgs := append([]string{"network", "acl", "create", l.ref(s.bridge), "--project", "default"}, lxdMeta()...)
	if _, err := l.lxc(ctx, aclArgs...); err != nil {
		return err
	}
	netArgs := append([]string{"network", "create", l.ref(s.bridge), "--project", "default",
		"ipv4.address=none", "ipv6.address=none", "security.acls=" + s.bridge,
		"security.acls.default.egress.action=reject", "security.acls.default.ingress.action=reject"}, lxdMeta()...)
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
	subnet := nextSubnet()
	e := &lxdEgress{proxy: s.name + "-p", ip: subnet.Addr().Next().Next(), agentIP: subnet.Addr().Next().Next().Next(), prefix: subnet.Bits()}
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
		_, _ = s.exec(ctx, Cmd{Argv: []string{"bash", "-c", canaryScript}, Root: true, Dir: "/"}, &out, &out)
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
	args := append([]string{"init", l.ref(img), p.inst(), "--project", lxdProject, "--storage", l.pool}, metaFlags()...)
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
		script := fmt.Sprintf(`timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%s' 2>/dev/null && echo "egress-reach ok=1" || echo "egress-reach ok=0"`, e.ip, proxyPort)
		_, _ = s.exec(ctx, Cmd{Argv: []string{"sh", "-c", script}, Root: true, Dir: "/"}, &out, &out)
		if !strings.Contains(out.String(), "egress-reach ok=1") {
			return fmt.Errorf("egress self-test: the sandbox cannot reach its proxy at %s on bridge %s. on a server that also runs docker, docker's FORWARD policy (with br_netfilter) drops ipv4 between instances on bridges it was not told about; an admin allows pats's bridges once:\n  iptables -I DOCKER-USER -i patsbr+ -j ACCEPT\n  iptables -I DOCKER-USER -o patsbr+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT", addr, s.bridge)
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

// metaFlags are lxdMeta as -c flags.
func metaFlags() []string {
	var out []string
	for _, kv := range lxdMeta() {
		out = append(out, "-c", kv)
	}
	return out
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
	penv["PATS_EGRESS_REFUSE"] = pool.String()
	penv["PATS_EGRESS_LISTEN"] = e.ip.String() + ":" + proxyPort
	var uid bytes.Buffer
	if code, err := p.exec(ctx, Cmd{Argv: []string{"id", "-u", lxdProxyUser}, Root: true, Dir: "/"}, &uid, io.Discard); err != nil || code != 0 {
		return fmt.Errorf("egress proxy user: %v", err)
	}
	p.uid = strings.TrimSpace(uid.String())

	var audit io.Writer = io.Discard
	if auditPath != "" {
		f, err := os.Create(auditPath)
		if err != nil {
			return fmt.Errorf("egress: audit log: %w", err)
		}
		e.auditF, audit = f, f
	}
	pctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	args := p.execArgs(Cmd{Argv: []string{"/usr/local/bin/pats", "egress", "run"}, Env: penv, Dir: "/"})
	if s.l.run != nil {
		// tests: the fake cli has no process to stream.
		return s.l.run(ctx, nil, audit, io.Discard, args...)
	}
	e.cmd = exec.CommandContext(pctx, s.l.bin, args...)
	e.cmd.Stdout = audit
	stderr, err := e.cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("egress: start proxy: %w", err)
	}
	if err := waitReady(stderr, readyWait); err != nil {
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
	script := fmt.Sprintf(`ip neigh replace %s lladdr %s dev pats0 nud permanent && ip route replace default via %s dev pats0 && { timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && echo "egress-hostile leak=1" || echo "egress-hostile leak=0"; }; ip route del default 2>/dev/null; ip neigh del %s dev pats0 2>/dev/null; true`,
		gw, st.Hwaddr, gw, gw)
	var res bytes.Buffer
	_, _ = s.exec(ctx, Cmd{Argv: []string{"sh", "-c", script}, Root: true, Dir: "/"}, &res, &res)
	if !strings.Contains(res.String(), "egress-hostile leak=0") {
		return fmt.Errorf("egress is not fail-closed under lxd: a root agent routing through the bridge's host side got out (%s)", strings.TrimSpace(res.String()))
	}
	return nil
}

// closeEgress stops the proxy process and container and drops the material.
func (s *lxdSession) closeEgress() {
	e := s.egress
	if e == nil {
		return
	}
	if e.cancel != nil {
		e.cancel()
	}
	if e.cmd != nil {
		_ = e.cmd.Wait()
	}
	if e.auditF != nil {
		_ = e.auditF.Close()
	}
	_, _ = s.l.lxc(context.Background(), "delete", s.l.ref(e.proxy), "--project", lxdProject, "--force")
	if e.m != nil {
		e.m.cleanup()
	}
}

// proxyImages caches the proxy image per server for the process: every
// proxied pair needs it, and resolving it means fetching the binary.
var (
	proxyImagesMu sync.Mutex
	proxyImages   = map[string]string{}
)

// proxyImage is the proxy container image on l's server, made once per
// process; see buildProxyImage.
func (l *lxd) proxyImage(ctx context.Context) (string, error) {
	if l.run != nil {
		return l.buildProxyImage(ctx)
	}
	// held across the build, so concurrent pairs wait for one build.
	proxyImagesMu.Lock()
	defer proxyImagesMu.Unlock()
	if alias, ok := proxyImages[l.o.Remote]; ok {
		return alias, nil
	}
	alias, err := l.buildProxyImage(ctx)
	if err == nil {
		proxyImages[l.o.Remote] = alias
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
	if err := proxyBinary(ctx, arch, filepath.Join(material, "pats")); err != nil {
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
	pl := &lxd{o: Options{Driver: "lxd", Remote: l.o.Remote}, bin: l.bin, run: l.run}
	alias, _, err := pl.image(ctx, LXDImageSpec{Base: lxdProxyBase, Material: material, Argv: []string{"/bin/sh", ProvisionMount + "/script"}}, io.Discard)
	return alias, err
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

// proxyBinary puts a linux pats for arch at dst: the file lxdProxyBinEnv
// names, this binary when it is one, else the published proxy image's.
func proxyBinary(ctx context.Context, arch, dst string) error {
	src := os.Getenv(lxdProxyBinEnv)
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
	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := fetchImageFile(deadline, img, arch, "pats", dst); err != nil {
		return fmt.Errorf("fetch the proxy binary from %s (or set %s): %w", img, lxdProxyBinEnv, err)
	}
	return nil
}
