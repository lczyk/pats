package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// the lxd driver drives a (usually remote) lxd server through the lxc cli and
// its configured remotes. instances and images live in the lxd project
// lxdProject; bridges and acls live in the default project, since a bridge
// network in another project needs ovn. everything a run makes carries
// user.pats.run (this process's token), user.pats.owner and user.pats.created.
//
// files go in and out over exec as tar streams: the workdir and every mount
// are pushed at start and the writable ones pulled back by Sync, so a remote
// server needs no shared filesystem.

// lxdProject is the lxd project pats works in.
const lxdProject = "pats"

// lxdAgentUser is the user the agent runs as: never root, since harnesses
// refuse their permission-bypass flags as root.
const lxdAgentUser = "pats"

// LXDUplinkEnv names an existing bridge to use as the uplink instead of the
// one pats makes, for a server where a new bridge cannot forward (a host
// whose docker drops forwarded traffic from bridges it was not told about).
const LXDUplinkEnv = "PATS_LXD_UPLINK"

// lxdUplink is the shared bridge only builders and egress proxies join.
const lxdUplink = "patsup"

type lxd struct {
	p    *process
	r    runner
	o    Options
	s    SandboxOptions // the sandbox's, zero on the driver itself
	pool string         // the storage pool the default project's default profile uses
}

func newLXD(p *process, r runner, o Options) *lxd { return &lxd{p: p, r: r, o: o} }

// lxc runs the cli with args and returns its trimmed combined output.
func (l *lxd) lxc(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := l.exec(ctx, nil, &out, &out, args...)
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("lxc %s: %w (%s)", strings.Join(args, " "), err, lastLine(s))
	}
	return s, nil
}

func (l *lxd) exec(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return l.r.Run(ctx, stdin, stdout, stderr, args...)
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ref names an instance, network or image on the configured remote.
func (l *lxd) ref(name string) string {
	if l.o.Remote == "" {
		return name
	}
	return l.o.Remote + ":" + name
}

// defaultRemote is the cli's default remote. a client's first ever command
// prints a welcome banner before its output, so the name is the last line.
func (l *lxd) defaultRemote(ctx context.Context) (string, error) {
	out, err := l.lxc(ctx, "remote", "get-default")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// lxdServer is what a process learns and makes once per lxd server: the
// pinned remote, the pool, and the run's marker.
type lxdServer struct {
	once   sync.Once
	err    error
	remote string
	pool   string
	stop   chan struct{} // ends the marker's lease renewal
	done   chan struct{} // closed once the renewal has ended
	leased time.Time     // the lease the marker was made with
	l      *lxd
}

// marker is the run's marker acl: the first thing a run makes on a server,
// holding its lease.
func (l *lxd) marker() string { return "pats-run-" + l.p.token }

// prepareServer readies the server once per process: see setupServer.
func (l *lxd) prepareServer(ctx context.Context) error {
	x := &l.p.lxd
	x.mu.Lock()
	srv := x.servers[l.o.Remote]
	if srv == nil {
		srv = &lxdServer{l: l}
		x.servers[l.o.Remote] = srv
	}
	x.mu.Unlock()
	srv.once.Do(func() {
		srv.leased = l.p.clock.Now()
		srv.remote, srv.pool, srv.err = l.setupServer(ctx)
		if srv.err == nil {
			srv.stop, srv.done = make(chan struct{}), make(chan struct{})
			go srv.renew(l.p.clock.NewTicker(l.p.t.leaseWake), srv.leased)
		}
	})
	l.o.Remote, l.pool = srv.remote, srv.pool
	return srv.err
}

// setupServer pins the remote (everything after names it, so a publish lands
// on the same server as the instance whatever the cli's default is), makes
// the run's marker before anything else of the run exists, makes the pats
// project on first use and finds the storage pool new instances get their
// root disk from.
func (l *lxd) setupServer(ctx context.Context) (remote, pool string, err error) {
	remote = l.o.Remote
	if remote == "" {
		if remote, err = l.defaultRemote(ctx); err != nil {
			return "", "", err
		}
	}
	// a setup that fails leaves no server for shutdown to end, so it removes
	// the marker it made, or a killed create may have made. one the create
	// found already there is left: the same server set up under another
	// remote name may own it (a create that failed only on the client looks
	// the same, and goes with its lease).
	ours := true
	defer func() {
		if err != nil && ours {
			_, _ = l.lxc(context.Background(), "network", "acl", "delete", remote+":"+l.marker(), "--project", "default")
		}
	}()
	marker := append([]string{"network", "acl", "create", remote + ":" + l.marker(), "--project", "default",
		"user.pats.lease=" + strconv.FormatInt(l.p.clock.Now().Unix(), 10)}, l.meta()...)
	if _, err := l.lxc(ctx, marker...); err != nil {
		if _, serr := l.lxc(ctx, "network", "acl", "show", remote+":"+l.marker(), "--project", "default"); serr != nil {
			return "", "", fmt.Errorf("lxd: run marker: %w", err)
		}
		ours = false
	}
	if _, err := l.lxc(ctx, "project", "show", remote+":"+lxdProject); err != nil {
		// networks stay in the default project: a bridge in another one
		// needs ovn.
		_, err := l.lxc(ctx, "project", "create", remote+":"+lxdProject,
			"-c", "features.images=true", "-c", "features.profiles=true",
			"-c", "features.networks=false", "-c", "features.storage.volumes=false")
		if err != nil {
			// another pats may have made it meanwhile.
			if _, serr := l.lxc(ctx, "project", "show", remote+":"+lxdProject); serr != nil {
				return "", "", fmt.Errorf("%w\nif this client's certificate is restricted, an admin creates the project once:\n  lxc project create %s:%s -c features.images=true -c features.profiles=true -c features.networks=false", err, remote, lxdProject)
			}
		}
	}
	pool, err = l.lxc(ctx, "profile", "device", "get", remote+":default", "root", "pool", "--project", "default")
	if err != nil || pool == "" {
		return "", "", fmt.Errorf("lxd: no root disk pool on the default profile: %w", err)
	}
	return remote, pool, nil
}

// renew keeps the marker's lease fresh while the process lives (it wakes
// every leaseWake and renews once leaseEvery has passed), and makes
// the marker again when it is gone: a setup of the same server under
// another remote name that failed removed it, or an admin did.
func (srv *lxdServer) renew(t ticker, last time.Time) {
	defer close(srv.done)
	p := srv.l.p
	defer t.Stop()
	for {
		select {
		case <-srv.stop:
			return
		case <-t.C():
			// on the wall clock, so a machine waking from sleep renews
			// within a wake.
			if p.clock.Now().Round(0).Sub(last.Round(0)) < p.t.leaseEvery {
				continue
			}
			last = p.clock.Now()
			// bounded: shutdown waits for a renewal in flight.
			ctx, cancel := context.WithTimeout(context.Background(), p.t.cliCleanup)
			lease := "user.pats.lease=" + strconv.FormatInt(p.clock.Now().Unix(), 10)
			ref := srv.remote + ":" + srv.l.marker()
			if out, err := srv.l.lxc(ctx, "network", "acl", "set", ref, lease, "--project", "default"); err != nil &&
				strings.Contains(strings.ToLower(out+err.Error()), "not found") {
				_, _ = srv.l.lxc(ctx, append([]string{"network", "acl", "create", ref, "--project", "default", lease}, srv.l.meta()...)...)
			}
			cancel()
		}
	}
}

func (p *process) shutdown() {
	defer p.endLease()
	x := &p.lxd
	x.mu.Lock()
	defer x.mu.Unlock()
	for k, srv := range x.servers {
		if srv.stop != nil {
			// a renewal in flight would make the marker again after the
			// delete: it ends first.
			close(srv.stop)
			<-srv.done
			ctx, cancel := context.WithTimeout(context.Background(), p.t.cliCleanup)
			_, _ = srv.l.lxc(ctx, "network", "acl", "delete", srv.remote+":"+srv.l.marker(), "--project", "default")
			cancel()
		}
		delete(x.servers, k)
	}
}

// lxdNameRe is what lxd allows in an instance name.
var lxdNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// lxdName derives an instance name from a workdir: unique per pair, as the
// workdir is.
func lxdName(workdir string) string {
	n := "pats-" + strings.TrimPrefix(lxdNameRe.ReplaceAllString(strings.ToLower(filepath.Base(workdir)), "-"), "pats-")
	if len(n) > 63 {
		sum := sha256.Sum256([]byte(n))
		n = n[:50] + "-" + hex.EncodeToString(sum[:])[:12]
	}
	return strings.TrimRight(n, "-")
}

// bridgeName is a slot bridge's name: linux caps interface names at 15.
func (l *lxd) bridgeName(instance string) string {
	sum := sha256.Sum256([]byte(instance + l.p.token))
	return "patsbr" + hex.EncodeToString(sum[:])[:8]
}

// meta are the config keys stamping a resource as this run's.
func (l *lxd) meta() []string {
	return []string{
		"user.pats.run=" + l.p.token,
		"user.pats.owner=" + l.p.owner(),
		"user.pats.created=" + strconv.FormatInt(l.p.clock.Now().Unix(), 10),
	}
}

// metaFlags are meta as -c flags.
func (l *lxd) metaFlags() []string {
	var out []string
	for _, kv := range l.meta() {
		out = append(out, "-c", kv)
	}
	return out
}

// openRanges are what an open sandbox may not reach: the server's lan, the
// host, other instances, link-local and multicast.
var openRanges = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "0.0.0.0/8",
	"fc00::/7", "fe80::/10", "ff00::/8", "::1/128",
}

func (l *lxd) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	return run(ctx, l, spec, stdout, stderr)
}

// Start makes the pair's instance from the image, stopped; gives it its nic
// (none, or an open bridge of its own); checks the expanded config adds no
// other; starts it; and pushes the workdir and mounts in.
func (l *lxd) Start(ctx context.Context, spec Spec) (Session, error) {
	if err := l.prepareServer(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workdir: %w", ErrLaunch, err)
	}
	if !spec.Egress.None {
		if err := l.o.machineCheck("lxd"); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
	}
	s := &lxdSession{l: l, name: lxdName(abs), workdir: abs, sudo: spec.Sudo}
	fail := func(err error) (Session, error) {
		s.Close()
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	img := l.s.Image
	if !strings.Contains(img, ":") {
		img = l.ref(img) // a pats image or fingerprint on the instance's own server
	}
	args := []string{"init", img, l.ref(s.name), "--project", lxdProject, "--storage", l.pool}
	if l.o.VM {
		// a vm honours a nic's name only with this.
		args = append(args, "--vm", "-c", "agent.nic_config=true")
	}
	args = append(args, l.metaFlags()...)
	if l.s.Limits.CPU > 0 {
		args = append(args, "-c", "limits.cpu="+strconv.Itoa(l.s.Limits.CPU))
	}
	if l.s.Limits.Memory != "" {
		args = append(args, "-c", "limits.memory="+l.s.Limits.Memory)
	}
	if l.s.Limits.Disk != "" {
		args = append(args, "-d", "root,size="+l.s.Limits.Disk)
	}
	for _, k := range sortedKeys(l.s.Config) {
		args = append(args, "-c", k+"="+l.s.Config[k])
	}
	if len(l.s.Profiles) > 0 {
		for _, p := range l.s.Profiles {
			args = append(args, "-p", p)
		}
	}
	// marked first: a cancelled or ambiguous init may still have made it,
	// and deleting a name that does not exist is harmless.
	s.created = true
	if _, err := l.lxc(ctx, args...); err != nil {
		return fail(err)
	}

	allowed := map[string]bool{}
	switch {
	case spec.Egress.Set:
		if err := s.startEgressNetwork(ctx); err != nil {
			return fail(err)
		}
		allowed[s.bridge] = true
	case !spec.Egress.None:
		if err := s.openNetwork(ctx); err != nil {
			return fail(err)
		}
		allowed[s.bridge] = true
	}
	if err := s.checkDevices(ctx, allowed); err != nil {
		return fail(err)
	}
	if _, err := l.lxc(ctx, "start", l.ref(s.name), "--project", lxdProject); err != nil {
		return fail(err)
	}
	// an open sandbox waits for its dhcp route; a proxied one has none.
	if err := s.waitReady(ctx, !spec.Egress.None && !spec.Egress.Set); err != nil {
		return fail(err)
	}
	if spec.Egress.SelfTest && !spec.Egress.None && !spec.Egress.Set {
		if err := s.reachesInternet(ctx); err != nil {
			return fail(err)
		}
	}
	if err := s.ensureAgentUser(ctx); err != nil {
		return fail(err)
	}
	if !spec.Egress.None && !spec.Egress.Set {
		if err := s.openBehindProxy(ctx); err != nil {
			return fail(err)
		}
	}
	if spec.Egress.Set {
		if err := s.startProxy(ctx, spec); err != nil {
			return fail(err)
		}
	}
	s.binds = append(s.binds, bind{host: abs, path: WorkMount})
	for _, m := range spec.Mounts {
		mh, err := filepath.Abs(m.Host)
		if err != nil {
			return fail(fmt.Errorf("resolve mount %s: %w", m.Host, err))
		}
		s.binds = append(s.binds, bind{host: mh, path: m.Container, ro: m.ReadOnly})
	}
	for _, b := range s.binds {
		if err := s.Push(ctx, b.host, b.path); err != nil {
			return fail(err)
		}
	}
	// the agent owns what it was handed; a root step may hand more over.
	if err := s.chownAgent(ctx, s.paths(false)...); err != nil {
		return fail(err)
	}
	return s, nil
}

// bind is a host path copied in at start and, unless ro, back by Sync.
type bind struct {
	host, path string
	ro         bool
}

type lxdSession struct {
	l       *lxd
	name    string
	workdir string
	sudo    bool
	binds   []bind
	env     map[string]string // the egress env (proxy, tls), set under a proxy policy
	// machineEnv is the machine's proxy env, in an open sandbox behind one.
	// it holds the upstream's credential, so it rides only on stdin.
	machineEnv map[string]string
	created    bool
	bridge     string // the pair's bridge and its acl, "" under egress none
	egress     *lxdEgress
	uid        string
	once       sync.Once
}

func (s *lxdSession) inst() string { return s.l.ref(s.name) }

// openNetwork gives the instance a bridge of its own with nat and an acl:
// the internet, but none of openRanges. bridge acls apply at the bridge-host
// boundary, which is exactly where the server's lan and other instances are.
func (s *lxdSession) openNetwork(ctx context.Context) error {
	l := s.l
	s.bridge = l.bridgeName(s.name)
	acl := s.bridge
	aclArgs := append([]string{"network", "acl", "create", l.ref(acl), "--project", "default"}, l.meta()...)
	if _, err := l.lxc(ctx, aclArgs...); err != nil {
		return err
	}
	ranges, err := l.openRejects(ctx)
	if err != nil {
		return err
	}
	if _, err := l.lxc(ctx, "network", "acl", "rule", "add", l.ref(acl), "egress", "--project", "default",
		"action=reject", "destination="+strings.Join(ranges, ",")); err != nil {
		return err
	}
	netArgs := []string{"network", "create", l.ref(s.bridge), "--project", "default",
		"ipv4.address=auto", "ipv4.nat=true", "ipv6.address=none",
		"security.acls=" + acl,
		"security.acls.default.egress.action=allow",
		"security.acls.default.ingress.action=reject"}
	netArgs = append(netArgs, l.meta()...)
	if _, err := l.lxc(ctx, netArgs...); err != nil {
		return err
	}
	_, err = l.lxc(ctx, "config", "device", "add", s.inst(), "eth0", "nic", "--project", lxdProject,
		"network="+s.bridge, "name=eth0")
	return err
}

// checkDevices refuses an instance whose expanded config, a profile's
// included, carries a nic other than the one pats gave it. checked while it is
// stopped: a nic up at boot is in use before anything could look.
func (s *lxdSession) checkDevices(ctx context.Context, allowed map[string]bool) error {
	out, err := s.l.lxc(ctx, "query", s.queryPath("/1.0/instances/"+s.name+"?project="+lxdProject))
	if err != nil {
		return err
	}
	var inst struct {
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
		ExpandedConfig  map[string]string            `json:"expanded_config"`
	}
	if err := json.Unmarshal([]byte(out), &inst); err != nil {
		return fmt.Errorf("lxd: read instance config: %w", err)
	}
	for name, d := range inst.ExpandedDevices {
		if d["type"] == "nic" && !allowed[d["network"]] {
			return fmt.Errorf("lxd: device %s (nic on %q) would bypass the sandbox's egress; pats gives the instance its only nic", name, d["network"]+d["parent"])
		}
	}
	for k := range inst.ExpandedConfig {
		if strings.HasPrefix(k, "raw.") {
			return fmt.Errorf("lxd: config %s (from a profile) is refused: raw config can add devices pats cannot check", k)
		}
	}
	return nil
}

// queryPath prefixes a raw api path with the remote, for lxc query.
func (s *lxdSession) queryPath(p string) string {
	if s.l.o.Remote == "" {
		return p
	}
	return s.l.o.Remote + ":" + p
}

// waitReady waits for exec to work (a vm's agent comes up after boot) and,
// with a network, for a default route.
func (s *lxdSession) waitReady(ctx context.Context, network bool) error {
	c, t := s.l.p.clock, s.l.p.t
	deadline := c.Now().Add(t.ready)
	p := probeReady
	if network {
		p = probeRoute
	}
	var last error
	for c.Now().Before(deadline) {
		var out bytes.Buffer
		code, err := s.exec(ctx, Cmd{Argv: p.argv(), Root: true, Dir: "/"}, &out, &out)
		if err == nil && code == 0 {
			return nil
		}
		last = fmt.Errorf("%v %s", err, strings.TrimSpace(out.String()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.After(t.readyPoll):
		}
	}
	return fmt.Errorf("lxd: %s not ready after %s: %v", s.name, t.ready, last)
}

// reachesInternet is the open sandbox's self-test: its bridge must forward.
// a server that also runs docker drops bridged traffic from bridges docker was
// not told about (its FORWARD policy, with br_netfilter), which fails every
// pair in its own words unless caught here.
func (s *lxdSession) reachesInternet(ctx context.Context) error {
	if c := s.l.o.Machine.chain(); c != nil {
		// behind a proxy there is no direct route to find: the upstream is
		// the way out.
		for _, u := range c.Upstreams() {
			var out bytes.Buffer
			_, _ = s.exec(ctx, Cmd{Argv: probeReach.argv(u.Hostname(), u.Port()), Root: true, Dir: "/"}, &out, &out)
			if reached, _ := reachVerdict(out.String()); !reached {
				return fmt.Errorf("lxd: the sandbox on bridge %s cannot reach the upstream %s. %s",
					s.bridge, proxy.Redact(u), dockerBridgeHint)
			}
		}
		return nil
	}
	if s.online(ctx) {
		return nil
	}
	return fmt.Errorf("lxd: the sandbox's bridge %s does not reach the internet. %s", s.bridge, dockerBridgeHint)
}

// online says whether the instance gets a tcp connection out to the
// internet, retrying a few times while a fresh nic settles. an instance with
// no bash cannot be probed and passes.
func (s *lxdSession) online(ctx context.Context) bool {
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-s.l.p.clock.After(s.l.p.t.onlineRetry):
			}
		}
		var out bytes.Buffer
		if code, err := s.exec(ctx, Cmd{Argv: probeOnline.argv(), Root: true, Dir: "/"}, &out, &out); err == nil && code == 0 {
			return true
		}
	}
	return false
}

// dockerBridgeHint is what an admin does once on a server whose docker drops
// pats's bridges. the sysctl route works because docker leaves the FORWARD
// policy alone when it finds forwarding already on; lxd's docs recommend it
// only from docker 28, before which an accept policy exposed container ports.
var dockerBridgeHint = "on a server that also runs docker, docker's FORWARD policy (with br_netfilter) drops traffic on bridges it was not told about. an admin lets pats's bridges through once, and makes it persist across reboots, either with\n" +
	"  iptables -I DOCKER-USER -i patsbr+ -j ACCEPT\n" +
	"  iptables -I DOCKER-USER -o patsbr+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n" +
	"  (and the same two for " + lxdUplink + ")\n" +
	"or, with docker 28 or later, by turning ipv4 forwarding on before docker starts (net.ipv4.ip_forward=1 in /etc/sysctl.d), so docker leaves the FORWARD policy at accept"

// ensureAgentUser makes the agent user when the image lacks it (a prebuilt
// image: pats provisioning makes it) and records its uid.
func (s *lxdSession) ensureAgentUser(ctx context.Context) error {
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: probeAgentUser.argv(), Root: true, Dir: "/"}, &out, &out)
	if err != nil || code != 0 {
		return fmt.Errorf("lxd: agent user: %v %s", err, strings.TrimSpace(out.String()))
	}
	if s.uid, err = uidVerdict(out.String()); err != nil {
		return fmt.Errorf("lxd: agent user: %w", err)
	}
	return nil
}

// paths are the in-instance paths of the binds; writable ones only when rw.
func (s *lxdSession) paths(rw bool) []string {
	var out []string
	for _, b := range s.binds {
		if !rw || !b.ro {
			out = append(out, b.path)
		}
	}
	return out
}

func (s *lxdSession) chownAgent(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	script := fmt.Sprintf(`for p; do [ -e "$p" ] || continue; chown -R %s:%s "$p" || exit 1; done`, s.uid, s.uid)
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: append([]string{"sh", "-c", script, "handover"}, paths...), Root: true, Dir: "/"}, &out, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("hand over %s: %w", strings.Join(paths, " "), err)
	}
	return nil
}

// HandOver gives what root left under paths to the agent user and sets the
// agent's sudo: a rule in /etc/sudoers.d when the pair allows it, none
// otherwise. one cached image serves both.
func (s *lxdSession) HandOver(ctx context.Context, paths ...string) error {
	if err := s.chownAgent(ctx, paths...); err != nil {
		return err
	}
	script := `rm -f /etc/sudoers.d/pats`
	if s.sudo {
		script = `[ -d /etc/sudoers.d ] || { echo "no sudo in the image" >&2; exit 1; }; echo '` + lxdAgentUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/pats && chmod 0440 /etc/sudoers.d/pats`
	}
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", script}, Root: true, Dir: "/"}, &out, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("sudo for the agent: %w", err)
	}
	return nil
}

// exec runs cmd in the instance without the cancel handling Exec adds.
func (s *lxdSession) exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	return s.execIn(ctx, nil, cmd, stdout, stderr)
}

// execArgs is the lxc exec argv for cmd, and the stdin to give it. with
// stdin free (nil), the env goes in on stdin as a script the command reads
// first, so no value is on lxc's command line for ps to read; a step that
// streams its own stdin (push, pull) carries only the egress env, which
// holds no secret, as --env flags.
func (s *lxdSession) execArgs(cmd Cmd, stdin io.Reader) ([]string, io.Reader) {
	user := s.uid
	if cmd.Root || user == "" {
		user = "0"
	}
	dir := cmd.Dir
	if dir == "" {
		dir = WorkMount
	}
	args := []string{"exec", s.inst(), "--project", lxdProject, "--force-noninteractive",
		"--user", user, "--group", user, "--cwd", dir}
	// the egress env reaches every step, root ones included: prepare runs
	// under the sandbox's policy too.
	env := mergeEnv(s.env, nil)
	if stdin == nil {
		env = mergeEnv(env, s.machineEnv)
	}
	env = mergeEnv(env, cmd.Env)
	argv := cmd.Argv
	switch {
	case len(env) == 0:
	case stdin == nil:
		argv, stdin = envArgv(argv), envScript(env)
	default:
		for _, kv := range sortedEnv(env) {
			args = append(args, "--env", kv)
		}
	}
	return append(append(args, "--"), argv...), stdin
}

func (s *lxdSession) execIn(ctx context.Context, stdin io.Reader, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	args, in := s.execArgs(cmd, stdin)
	return exitResult(s.l.exec(ctx, in, stdout, stderr, args...), "lxc exec")
}

// Exec runs cmd; on ctx cancel the agent's processes are killed and the
// instance kept, so Sync can still pull what it wrote. a cancelled root step
// stops the instance outright: setup failed, nothing is collected.
func (s *lxdSession) Exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	cancel := func() {
		bg := context.Background()
		if cmd.Root {
			_, _ = s.l.lxc(bg, "stop", s.inst(), "--project", lxdProject, "--force")
		} else {
			_, _ = s.exec(bg, Cmd{Argv: []string{"pkill", "-KILL", "-u", s.uid}, Root: true, Dir: "/"}, io.Discard, io.Discard)
		}
	}
	args, in := s.execArgs(cmd, nil)
	return exitResult(s.l.p.execCancellable(ctx, s.l.r, in, stdout, stderr, cancel, args...), "lxc exec")
}

// Push streams hostPath into path as a tar over exec. a directory's contents
// land in path; a file lands at path.
func (s *lxdSession) Push(ctx context.Context, hostPath, path string) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	local := make(chan error, 1)
	go func() {
		err := writeTar(pw, hostPath, fi)
		pw.CloseWithError(err)
		local <- err
	}()
	dest := path
	if !fi.IsDir() {
		dest = filepath.Dir(path)
	}
	script := `mkdir -p "$1" && tar -x -m -C "$1"`
	if !fi.IsDir() {
		script = `mkdir -p "$1" && tar -x -m -C "$1" && { [ "$1/` + filepath.Base(hostPath) + `" = "$2" ] || mv "$1/` + filepath.Base(hostPath) + `" "$2"; }`
	}
	var out bytes.Buffer
	code, err := s.execIn(ctx, pr, Cmd{Argv: []string{"sh", "-c", script, "push", dest, path}, Root: true, Dir: "/"}, &out, &out)
	pr.Close()
	// the local side failing is what made the remote tar complain, so it
	// leads the error.
	if lerr := <-local; lerr != nil && !errors.Is(lerr, io.ErrClosedPipe) {
		return fmt.Errorf("push %s: %w (in the instance: %s)", hostPath, lerr, strings.TrimSpace(out.String()))
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("push %s: %w", hostPath, err)
	}
	return nil
}

// writeTar writes hostPath (a dir's contents, or one file) as a tar stream.
// only directories, regular files and symlinks travel: a socket an agent or
// its tools left in the home (which archive/tar refuses), a fifo or a device
// node has no meaning on the other side.
func writeTar(w io.Writer, hostPath string, fi fs.FileInfo) error {
	tw := tar.NewWriter(w)
	add := func(p, name string) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = name
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	}
	if !fi.IsDir() {
		if err := add(hostPath, filepath.Base(hostPath)); err != nil {
			return err
		}
		return tw.Close()
	}
	err := filepath.WalkDir(hostPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(hostPath, p)
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			name += "/"
		}
		return add(p, name)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Pull streams path out as a tar over exec into hostDir: a directory's
// contents land in hostDir.
func (s *lxdSession) Pull(ctx context.Context, path, hostDir string) error {
	pr, pw := io.Pipe()
	var errb bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := s.execIn(ctx, nil, Cmd{Argv: []string{"tar", "-c", "-C", path, "."}, Root: true, Dir: "/"}, pw, &errb)
		if err == nil && code != 0 {
			err = fmt.Errorf("tar exited %d: %s", code, strings.TrimSpace(errb.String()))
		}
		pw.CloseWithError(err)
		done <- err
	}()
	err := readTar(pr, hostDir)
	// tar's trailing padding follows the end-of-archive marker readTar
	// stops at; drain it so the writer does not fail on a closed pipe.
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	if perr := <-done; perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return fmt.Errorf("pull %s: %w", path, err)
	}
	return nil
}

// readTar extracts a tar stream into dir as the current user. the stream is
// the word of the tar inside an instance, not pats's own: a name or a hard
// link that leaves dir is refused, and every entry is written through dir's
// root, so nothing lands past a symlink that leaves dir (dir itself may be
// reached through one). symlinks are made last, so only another symlink can
// meet one the stream made.
func readTar(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	links := map[string]string{} // a symlink's path -> its target
	skipped := map[string]bool{} // what does not travel: fifos, devices
	files := map[string]bool{}   // the regular files this stream made
	// the directories known to be there, so each is looked for once: a root
	// walks a path anew on every call. forgotten when an entry is removed,
	// which may have been one of them.
	made := map[string]bool{}
	parent := func(rel string) error {
		dir := path.Dir(rel)
		if made[dir] {
			return nil
		}
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		made[dir] = true
		return nil
	}
	remove := func(rel string) {
		clear(made)
		_ = root.Remove(rel)
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, err := tarRel(h.Name)
		if err != nil {
			return err
		}
		switch {
		case rel == ".":
			continue
		case h.Typeflag == tar.TypeSymlink:
			links[rel] = h.Linkname
			delete(skipped, rel)
			delete(files, rel)
			continue
		case h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeLink:
			skipped[rel] = true
			delete(links, rel)
			delete(files, rel)
			continue
		}
		// tar names a file's second path by its first, which came before it.
		// a name linked to itself is the entry it already is.
		src := ""
		if h.Typeflag == tar.TypeLink {
			if src, err = tarRel(h.Linkname); err != nil || src == "." {
				return fmt.Errorf("tar entry %q: hard link to %q %w", h.Name, h.Linkname, errEscape)
			}
			if src == rel {
				continue
			}
		}
		// a later entry of a name replaces an earlier one, as tar unpacks.
		delete(links, rel)
		delete(skipped, rel)
		delete(files, rel)
		if err := parent(rel); err != nil {
			return fmt.Errorf("tar entry %q: %w", h.Name, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err := root.Mkdir(rel, 0o755)
			if errors.Is(err, fs.ErrExist) {
				// a directory already there is kept. a link or a file gives
				// way: Chmod would follow a link to the directory it names.
				if fi, lerr := root.Lstat(rel); lerr == nil && fi.IsDir() {
					err = nil
				} else if err = root.Remove(rel); err == nil {
					clear(made)
					err = root.Mkdir(rel, 0o755)
				}
			}
			if err != nil {
				return err
			}
			_ = root.Chmod(rel, fs.FileMode(h.Mode).Perm()|0o700)
			made[rel] = true
		case tar.TypeReg:
			perm := fs.FileMode(h.Mode).Perm() | 0o600
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			if errors.Is(err, fs.ErrExist) {
				remove(rel)
				f, err = root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			}
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			files[rel] = true
		case tar.TypeLink:
			// the second name of a symlink is that symlink again, and of
			// what does not travel, left out with it.
			if target, isLink := links[src]; isLink {
				links[rel] = target
				continue
			}
			if skipped[src] {
				skipped[rel] = true
				continue
			}
			// only to a file this stream made: one that was there before
			// may be another name of a file outside dir.
			if !files[src] {
				return fmt.Errorf("tar entry %q: hard link to %q, which is no file of this archive", h.Name, h.Linkname)
			}
			err := root.Link(src, rel)
			if errors.Is(err, fs.ErrExist) {
				remove(rel)
				err = root.Link(src, rel)
			}
			if err != nil {
				return err
			}
			files[rel] = true
		}
	}
	rels := make([]string, 0, len(links))
	for rel := range links {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := parent(rel); err != nil {
			return fmt.Errorf("tar entry %q: %w", rel, err)
		}
		err := root.Symlink(links[rel], rel)
		if errors.Is(err, fs.ErrExist) {
			remove(rel)
			err = root.Symlink(links[rel], rel)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// tarRel is a tar entry's name as a clean path relative to where it is
// unpacked, "." for that directory itself; a name that leaves it is refused.
func tarRel(name string) (string, error) {
	if len(name) > maxPath {
		return "", fmt.Errorf("tar entry %.40q... %w", name, errLong)
	}
	rel := path.Clean(filepath.ToSlash(name))
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("tar entry %q %w", name, errEscape)
	}
	return rel, nil
}

// Sync pulls every writable bind back to its host path, replacing what was
// there, so the host sees what the agent left: collect reads it.
func (s *lxdSession) Sync(ctx context.Context) error {
	for _, b := range s.binds {
		if b.ro {
			continue
		}
		if err := clearDir(b.host); err != nil {
			return err
		}
		if err := s.Pull(ctx, b.path, b.host); err != nil {
			return err
		}
	}
	return nil
}

// clearDir empties dir, keeping dir itself.
func clearDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Close deletes the instance, then its bridge and acl.
func (s *lxdSession) Close() error {
	s.once.Do(func() {
		bg := context.Background()
		s.closeEgress()
		if s.created {
			_ = s.l.deleteInstance(s.name)
		}
		if s.bridge != "" {
			_, _ = s.l.lxc(bg, "network", "delete", s.l.ref(s.bridge), "--project", "default")
			_, _ = s.l.lxc(bg, "network", "acl", "delete", s.l.ref(s.bridge), "--project", "default")
		}
	})
	return nil
}

// deleteInstance removes an instance of the pats project for good. killing
// lxc does not stop the server's half of an init, so an instance whose init
// was cut short (a deadline, ctrl+c) can appear after a delete has already
// said it is not there; while an operation on it still runs, the delete is
// retried until the operation is over or instanceGone has passed. the
// operations are read before each delete, not after: a create that was over
// before the delete cannot finish after it (docs/spec/fizz/lxd_delete.fizz).
func (l *lxd) deleteInstance(name string) error {
	t := l.p.t
	ctx, cancel := context.WithTimeout(context.Background(), t.instanceGone)
	defer cancel()
	for {
		busy := l.instanceBusy(ctx, name)
		_, err := l.lxc(ctx, "delete", l.ref(name), "--project", lxdProject, "--force")
		if !busy {
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
				err = nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance %s: a server operation on it outlived %s; pats clean removes it once this run has ended", name, t.instanceGone)
		case <-l.p.clock.After(t.deleteRetry):
		}
	}
}

// instanceBusy says whether a running operation on the server names the
// instance.
func (l *lxd) instanceBusy(ctx context.Context, name string) bool {
	var ops struct {
		Running []lxdOperation `json:"running"`
	}
	if l.query(ctx, "/1.0/operations?project="+lxdProject+"&recursion=1", &ops) != nil {
		return false
	}
	for _, op := range ops.Running {
		if op.names("/1.0/instances/" + name) {
			return true
		}
	}
	return false
}

// lxdOperation is what an operation says of what it works on: lxd 5 lists
// it under resources, lxd 6 leaves those empty for a create and names the
// instance in metadata.entity_url.
type lxdOperation struct {
	Resources map[string][]string `json:"resources"`
	Metadata  map[string]any      `json:"metadata"`
}

// names says whether the operation works on the entity at url (with or
// without its project query).
func (op lxdOperation) names(url string) bool {
	match := func(r string) bool { return r == url || strings.HasPrefix(r, url+"?") }
	for _, rs := range op.Resources {
		for _, r := range rs {
			if match(r) {
				return true
			}
		}
	}
	e, _ := op.Metadata["entity_url"].(string)
	return match(e)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
