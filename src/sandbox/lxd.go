package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// LXDDriver is the lxd driver's name in a config and a pin target.
const LXDDriver = "lxd"

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

func init() {
	registerDriver(LXDDriver, driverDef{
		open: func(p *process, r runner, o Options) Driver {
			if r == nil {
				r = lxcCLI
			}
			return newLXD(p, r, o)
		},
		// a pin names its remote, and lxd has no daemon id to hold it to.
		reopen: func(_ context.Context, _, rest, _ string) (Driver, bool) {
			d, err := Open(Options{Driver: LXDDriver, Remote: rest}.WithEnv())
			return d, err == nil
		},
	})
}

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
