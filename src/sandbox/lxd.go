package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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

// lxdUplinkEnv names an existing bridge to use as the uplink instead of the
// one pats makes, for a server where a new bridge cannot forward (a host
// whose docker drops forwarded traffic from bridges it was not told about).
const lxdUplinkEnv = "PATS_LXD_UPLINK"

// lxdUplink is the shared bridge only builders and egress proxies join.
const lxdUplink = "patsup"

// lxdRunToken names this pats process in everything it makes on the server.
var lxdRunToken = func() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// lxdOwner is the host and uid that made a resource.
func lxdOwner() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getuid(), clientID())
}

type lxd struct {
	o   Options
	bin string
	// run executes the cli; swappable in tests (nil -> real exec).
	run func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error

	setup    sync.Once
	setupErr error
	pool     string // the storage pool the default project's default profile uses
}

func newLXD(o Options) *lxd { return &lxd{o: o, bin: "lxc"} }

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
	if l.run != nil {
		return l.run(ctx, stdin, stdout, stderr, args...)
	}
	cmd := exec.CommandContext(ctx, l.bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
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

// lxdServer is what a process learns and makes once per lxd server: the
// pinned remote, the pool, and the run's marker.
type lxdServer struct {
	once   sync.Once
	err    error
	remote string
	pool   string
	stop   chan struct{} // ends the marker's lease renewal
	l      *lxd
}

var (
	lxdServersMu sync.Mutex
	lxdServers   = map[string]*lxdServer{}
)

// leaseEvery is how often a live run renews its marker's lease; leaseStale
// is how old a lease must be before `pats clean` takes the run as gone.
const (
	leaseEvery = 2 * time.Minute
	leaseStale = 10 * time.Minute
)

// markerName is the run's marker acl: the first thing a run makes on a
// server, holding its lease.
func markerName() string { return "pats-run-" + lxdRunToken }

// prepareServer readies the server once per process (per driver under
// tests, whose fake cli is per driver): see setupServer.
func (l *lxd) prepareServer(ctx context.Context) error {
	if l.run != nil {
		l.setup.Do(func() {
			l.o.Remote, l.pool, l.setupErr = l.setupServer(ctx, false)
		})
		return l.setupErr
	}
	lxdServersMu.Lock()
	srv := lxdServers[l.o.Remote]
	if srv == nil {
		srv = &lxdServer{l: l}
		lxdServers[l.o.Remote] = srv
	}
	lxdServersMu.Unlock()
	srv.once.Do(func() {
		srv.remote, srv.pool, srv.err = l.setupServer(ctx, true)
		if srv.err == nil {
			srv.stop = make(chan struct{})
			go srv.renew()
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
func (l *lxd) setupServer(ctx context.Context, real bool) (remote, pool string, err error) {
	remote = l.o.Remote
	if remote == "" {
		if remote, err = l.lxc(ctx, "remote", "get-default"); err != nil {
			return "", "", err
		}
	}
	marker := append([]string{"network", "acl", "create", remote + ":" + markerName(), "--project", "default",
		"user.pats.lease=" + strconv.FormatInt(time.Now().Unix(), 10)}, lxdMeta()...)
	if _, err := l.lxc(ctx, marker...); err != nil {
		// the same server reached under a second name already made it.
		if _, serr := l.lxc(ctx, "network", "acl", "show", remote+":"+markerName(), "--project", "default"); serr != nil {
			return "", "", fmt.Errorf("lxd: run marker: %w", err)
		}
	}
	if _, err := l.lxc(ctx, "project", "show", remote+":"+lxdProject); err != nil {
		// networks stay in the default project: a bridge in another one
		// needs ovn.
		if _, err := l.lxc(ctx, "project", "create", remote+":"+lxdProject,
			"-c", "features.images=true", "-c", "features.profiles=true",
			"-c", "features.networks=false", "-c", "features.storage.volumes=false"); err != nil {
			return "", "", fmt.Errorf("%w\nif this client's certificate is restricted, an admin creates the project once:\n  lxc project create %s:%s -c features.images=true -c features.profiles=true -c features.networks=false", err, remote, lxdProject)
		}
	}
	pool, err = l.lxc(ctx, "profile", "device", "get", remote+":default", "root", "pool", "--project", "default")
	if err != nil || pool == "" {
		return "", "", fmt.Errorf("lxd: no root disk pool on the default profile: %w", err)
	}
	return remote, pool, nil
}

// renew keeps the marker's lease fresh while the process lives.
func (srv *lxdServer) renew() {
	t := time.NewTicker(leaseEvery)
	defer t.Stop()
	for {
		select {
		case <-srv.stop:
			return
		case <-t.C:
			_, _ = srv.l.lxc(context.Background(), "network", "acl", "set", srv.remote+":"+markerName(),
				"user.pats.lease="+strconv.FormatInt(time.Now().Unix(), 10), "--project", "default")
		}
	}
}

// ShutdownLXD ends this process's runs on every lxd server it used: lease
// renewal stops and the marker goes. what the run made is already gone (each
// session closes its own); a crash skips this, and `pats clean` sees the
// lease go stale.
func ShutdownLXD() {
	lxdServersMu.Lock()
	defer lxdServersMu.Unlock()
	for k, srv := range lxdServers {
		if srv.stop != nil {
			close(srv.stop)
			_, _ = srv.l.lxc(context.Background(), "network", "acl", "delete", srv.remote+":"+markerName(), "--project", "default")
		}
		delete(lxdServers, k)
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
func bridgeName(instance string) string {
	sum := sha256.Sum256([]byte(instance + lxdRunToken))
	return "patsbr" + hex.EncodeToString(sum[:])[:8]
}

// lxdMeta are the -c flags stamping a resource as this run's.
func lxdMeta() []string {
	return []string{
		"user.pats.run=" + lxdRunToken,
		"user.pats.owner=" + lxdOwner(),
		"user.pats.created=" + strconv.FormatInt(time.Now().Unix(), 10),
	}
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
	s := &lxdSession{l: l, name: lxdName(abs), workdir: abs, sudo: spec.Sudo}
	fail := func(err error) (Session, error) {
		s.Close()
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	args := []string{"init", l.o.Image, l.ref(s.name), "--project", lxdProject, "--storage", l.pool}
	if l.o.VM {
		// a vm honours a nic's name only with this.
		args = append(args, "--vm", "-c", "agent.nic_config=true")
	}
	for _, kv := range lxdMeta() {
		args = append(args, "-c", kv)
	}
	if l.o.Limits.CPU > 0 {
		args = append(args, "-c", "limits.cpu="+strconv.Itoa(l.o.Limits.CPU))
	}
	if l.o.Limits.Memory != "" {
		args = append(args, "-c", "limits.memory="+l.o.Limits.Memory)
	}
	if l.o.Limits.Disk != "" {
		args = append(args, "-d", "root,size="+l.o.Limits.Disk)
	}
	for _, k := range sortedKeys(l.o.Config) {
		args = append(args, "-c", k+"="+l.o.Config[k])
	}
	if len(l.o.Profiles) > 0 {
		for _, p := range l.o.Profiles {
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
	created bool
	bridge  string // the pair's bridge and its acl, "" under egress none
	egress  *lxdEgress
	uid     string
	once    sync.Once
}

func (s *lxdSession) inst() string { return s.l.ref(s.name) }

// openNetwork gives the instance a bridge of its own with nat and an acl:
// the internet, but none of openRanges. bridge acls apply at the bridge-host
// boundary, which is exactly where the server's lan and other instances are.
func (s *lxdSession) openNetwork(ctx context.Context) error {
	s.bridge = bridgeName(s.name)
	acl := s.bridge
	l := s.l
	aclArgs := []string{"network", "acl", "create", l.ref(acl), "--project", "default"}
	for _, kv := range lxdMeta() {
		aclArgs = append(aclArgs, kv)
	}
	if _, err := l.lxc(ctx, aclArgs...); err != nil {
		return err
	}
	if _, err := l.lxc(ctx, "network", "acl", "rule", "add", l.ref(acl), "egress", "--project", "default",
		"action=reject", "destination="+strings.Join(openRanges, ",")); err != nil {
		return err
	}
	netArgs := []string{"network", "create", l.ref(s.bridge), "--project", "default",
		"ipv4.address=auto", "ipv4.nat=true", "ipv6.address=none",
		"security.acls=" + acl,
		"security.acls.default.egress.action=allow",
		"security.acls.default.ingress.action=reject"}
	netArgs = append(netArgs, lxdMeta()...)
	if _, err := l.lxc(ctx, netArgs...); err != nil {
		return err
	}
	_, err := l.lxc(ctx, "config", "device", "add", s.inst(), "eth0", "nic", "--project", lxdProject,
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
	deadline := time.Now().Add(3 * time.Minute)
	probe := []string{"true"}
	if network {
		probe = []string{"sh", "-c", "ip -4 route show default | grep -q ."}
	}
	var last error
	for time.Now().Before(deadline) {
		var out bytes.Buffer
		code, err := s.exec(ctx, Cmd{Argv: probe, Root: true, Dir: "/"}, &out, &out)
		if err == nil && code == 0 {
			return nil
		}
		last = fmt.Errorf("%v %s", err, strings.TrimSpace(out.String()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("lxd: %s not ready after 3m: %v", s.name, last)
}

// reachesInternet is the open sandbox's self-test: its bridge must forward.
// a server that also runs docker drops bridged traffic from bridges docker was
// not told about (its FORWARD policy, with br_netfilter), which fails every
// pair in its own words unless caught here.
func (s *lxdSession) reachesInternet(ctx context.Context) error {
	if s.online(ctx) {
		return nil
	}
	return fmt.Errorf("lxd: the sandbox's bridge %s does not reach the internet. %s", s.bridge, dockerBridgeHint)
}

// online says whether the instance gets a tcp connection out to the
// internet, retrying a few times while a fresh nic settles. an instance with
// no bash cannot be probed and passes.
func (s *lxdSession) online(ctx context.Context) bool {
	probe := `command -v bash >/dev/null 2>&1 || exit 0; timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null || timeout 5 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>/dev/null`
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Second):
			}
		}
		var out bytes.Buffer
		if code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", probe}, Root: true, Dir: "/"}, &out, &out); err == nil && code == 0 {
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
	script := `id -u ` + lxdAgentUser + ` >/dev/null 2>&1 || useradd -m -s /bin/bash ` + lxdAgentUser + `; id -u ` + lxdAgentUser
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", script}, Root: true, Dir: "/"}, &out, &out)
	if err != nil || code != 0 {
		return fmt.Errorf("lxd: agent user: %v %s", err, strings.TrimSpace(out.String()))
	}
	s.uid = lastLine(strings.TrimSpace(out.String()))
	if _, err := strconv.Atoi(s.uid); err != nil {
		return fmt.Errorf("lxd: agent user uid %q", s.uid)
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

func (s *lxdSession) execArgs(cmd Cmd) []string {
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
	env := map[string]string{}
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range cmd.Env {
		env[k] = v
	}
	for _, kv := range sortedEnv(env) {
		args = append(args, "--env", kv)
	}
	return append(append(args, "--"), cmd.Argv...)
}

func (s *lxdSession) execIn(ctx context.Context, stdin io.Reader, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	err := s.l.exec(ctx, stdin, stdout, stderr, s.execArgs(cmd)...)
	if err == nil {
		return 0, nil
	}
	if e, ok := asExitError(err); ok {
		return e.ExitCode(), nil
	}
	return -1, fmt.Errorf("lxc exec: %w", err)
}

// Exec runs cmd; on ctx cancel the agent's processes are killed and the
// instance kept, so Sync can still pull what it wrote. a cancelled root step
// stops the instance outright: setup failed, nothing is collected.
func (s *lxdSession) Exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error) {
	if s.l.run != nil {
		return s.execIn(ctx, nil, cmd, stdout, stderr)
	}
	x := exec.CommandContext(ctx, s.l.bin, s.execArgs(cmd)...)
	x.Stdout, x.Stderr = stdout, stderr
	x.Cancel = func() error {
		bg := context.Background()
		if cmd.Root {
			_, _ = s.l.lxc(bg, "stop", s.inst(), "--project", lxdProject, "--force")
		} else {
			_, _ = s.exec(bg, Cmd{Argv: []string{"pkill", "-KILL", "-u", s.uid}, Root: true, Dir: "/"}, io.Discard, io.Discard)
		}
		return x.Process.Signal(syscall.SIGTERM)
	}
	x.WaitDelay = 10 * time.Second
	err := x.Run()
	if err == nil {
		return 0, nil
	}
	if e, ok := asExitError(err); ok {
		return e.ExitCode(), nil
	}
	return -1, fmt.Errorf("lxc exec: %w", err)
}

// Push streams hostPath into path as a tar over exec. a directory's contents
// land in path; a file lands at path.
func (s *lxdSession) Push(ctx context.Context, hostPath, path string) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTar(pw, hostPath, fi))
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
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("push %s: %w", hostPath, err)
	}
	return nil
}

// writeTar writes hostPath (a dir's contents, or one file) as a tar stream.
func writeTar(w io.Writer, hostPath string, fi fs.FileInfo) error {
	tw := tar.NewWriter(w)
	add := func(p, name string) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
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

// readTar extracts a tar stream into dir as the current user: names that
// leave dir are refused, and symlinks are made last so nothing is written
// through one.
func readTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	type link struct{ path, target string }
	var links []link
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q leaves the destination", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			_ = os.Chmod(p, fs.FileMode(h.Mode).Perm()|0o700)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			_ = os.Remove(p)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm()|0o600)
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
		case tar.TypeSymlink:
			links = append(links, link{p, h.Linkname})
		}
	}
	sort.Slice(links, func(i, j int) bool { return links[i].path < links[j].path })
	for _, ln := range links {
		if err := os.MkdirAll(filepath.Dir(ln.path), 0o755); err != nil {
			return err
		}
		_ = os.Remove(ln.path)
		if err := os.Symlink(ln.target, ln.path); err != nil {
			return err
		}
	}
	return nil
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

// instanceGone bounds deleteInstance's wait for a server operation still
// working on the instance.
var instanceGone = 5 * time.Minute

// deleteInstance removes an instance of the pats project for good. killing
// lxc does not stop the server's half of an init, so an instance whose init
// was cut short (a deadline, ctrl+c) can appear after a delete has already
// said it is not there; while an operation on it still runs, the delete is
// retried until the operation is over or instanceGone has passed. the
// operations are read before each delete, not after: a create that was over
// before the delete cannot finish after it (docs/spec/fizz/lxd_delete.fizz).
func (l *lxd) deleteInstance(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), instanceGone)
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
			return fmt.Errorf("instance %s: a server operation on it outlived %s; pats clean removes it once this run has ended", name, instanceGone)
		case <-time.After(2 * time.Second):
		}
	}
}

// instanceBusy says whether a running operation on the server names the
// instance.
func (l *lxd) instanceBusy(ctx context.Context, name string) bool {
	var ops struct {
		Running []struct {
			Resources map[string][]string `json:"resources"`
		} `json:"running"`
	}
	if l.query(ctx, "/1.0/operations?project="+lxdProject+"&recursion=1", &ops) != nil {
		return false
	}
	want := "/1.0/instances/" + name
	for _, op := range ops.Running {
		for _, r := range op.Resources["instances"] {
			if r == want || strings.HasPrefix(r, want+"?") {
				return true
			}
		}
	}
	return false
}

// ServerCPUs is the lxd server's cpu count, 0 when it cannot be read: -j auto
// sizes the run by the machine the pairs run on.
func ServerCPUs(ctx context.Context, remote string) int {
	l := newLXD(Options{Remote: remote})
	p := "/1.0/resources"
	if remote != "" {
		p = remote + ":" + p
	}
	out, err := l.lxc(ctx, "query", p)
	if err != nil {
		return 0
	}
	var res struct {
		CPU struct {
			Total int `json:"total"`
		} `json:"cpu"`
	}
	if json.Unmarshal([]byte(out), &res) != nil {
		return 0
	}
	return res.CPU.Total
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
