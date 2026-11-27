package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		err = fmt.Errorf("lxc %s: %w (%s)", strings.Join(args, " "), err, lastLine(s))
		// an exec's output is the remote command's too: it is no evidence.
		return s, asPlace(err, args[0] != "exec" && transientText(s))
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
	out, err := l.lxcIdem(ctx, "remote", "get-default")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// lxdServer is what a process learns and makes once per lxd server: the
// pinned remote, the pool, and the run's marker.
type lxdServer struct {
	mu     sync.Mutex // one setup at a time
	ready  bool       // set up: a failed setup is not kept, the next start tries again
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
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.ready {
		srv.leased = l.p.clock.Now()
		remote, pool, err := l.setupServer(ctx)
		if err != nil {
			return err
		}
		srv.remote, srv.pool, srv.ready = remote, pool, true
		srv.stop, srv.done = make(chan struct{}), make(chan struct{})
		go srv.renew(l.p.clock.NewTicker(l.p.t.leaseWake), srv.leased)
	}
	l.o.Remote, l.pool = srv.remote, srv.pool
	return nil
}

// setupServer pins the remote (everything after names it, so a publish lands
// on the same server as the instance whatever the cli's default is), makes
// the run's marker before anything else of the run exists, makes the pats
// project on first use and finds the storage pool new instances get their
// root disk from. every call is a read, an idempotent write or a create by
// name, so each is tried again on a lost link: a create that meets "already
// exists" is an earlier try's, landed with its answer lost (the marker's name
// carries the run's token; the project is the one every run shares).
//
// a setup that fails leaves the marker: whether this setup made it may not be
// known, and the same server set up under another remote name may have
// adopted it. a marker with nothing beside it and a lease never renewed is
// pats clean's once the lease is stale.
func (l *lxd) setupServer(ctx context.Context) (remote, pool string, err error) {
	remote = l.o.Remote
	if remote == "" {
		if remote, err = l.defaultRemote(ctx); err != nil {
			return "", "", err
		}
	}
	lease := "user.pats.lease=" + strconv.FormatInt(l.p.clock.Now().Unix(), 10)
	marker := append([]string{"network", "acl", "create", remote + ":" + l.marker(), "--project", "default", lease}, l.meta()...)
	if err := l.createByName(ctx, marker...); err != nil {
		if _, serr := l.lxcIdem(ctx, "network", "acl", "show", remote+":"+l.marker(), "--project", "default"); serr != nil {
			return "", "", fmt.Errorf("lxd: run marker: %w (and reading it: %w)", err, serr)
		}
	}
	// the marker may be an earlier setup's, found on any try, its lease stale.
	if _, err := l.lxcIdem(ctx, "network", "acl", "set", remote+":"+l.marker(), lease, "--project", "default"); err != nil {
		return "", "", fmt.Errorf("lxd: run marker: renew its lease: %w", err)
	}
	if _, err := l.lxcIdem(ctx, "project", "show", remote+":"+lxdProject); errors.Is(err, ErrPlace) {
		return "", "", err
	} else if err != nil {
		// networks stay in the default project: a bridge in another one
		// needs ovn.
		err := l.createByName(ctx, "project", "create", remote+":"+lxdProject,
			"-c", "features.images=true", "-c", "features.profiles=true",
			"-c", "features.networks=false", "-c", "features.storage.volumes=false")
		if err != nil {
			// another pats may have made it meanwhile.
			if _, serr := l.lxcIdem(ctx, "project", "show", remote+":"+lxdProject); serr != nil {
				return "", "", fmt.Errorf("%w (and reading it: %w)\nif this client's certificate is restricted, an admin creates the project once:\n  lxc project create %s:%s -c features.images=true -c features.profiles=true -c features.networks=false", err, serr, remote, lxdProject)
			}
		}
	}
	pool, err = l.lxcIdem(ctx, "profile", "device", "get", remote+":default", "root", "pool", "--project", "default")
	if err != nil || pool == "" {
		return "", "", fmt.Errorf("lxd: no root disk pool on the default profile: %w", err)
	}
	return remote, pool, nil
}

// createByName runs a create of a thing whose name says whose it is, tried
// again on a lost link; "already exists" after a lost answer is the earlier
// try's.
func (l *lxd) createByName(ctx context.Context, args ...string) error {
	tries := 0
	return l.retry(ctx, func() error {
		tries++
		_, err := l.lxc(ctx, args...)
		if err != nil && tries > 1 && strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		return err
	})
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
			if out, err := srv.l.lxcIdem(ctx, "network", "acl", "set", ref, lease, "--project", "default"); err != nil &&
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
	// before the markers: a run's leftovers keep it listed for clean.
	ctx, cancel := context.WithTimeout(context.Background(), p.t.cliCleanup)
	x.retryDeferred(ctx)
	cancel()
	x.mu.Lock()
	defer x.mu.Unlock()
	for k, srv := range x.servers {
		srv.mu.Lock()
		if srv.stop != nil {
			// a renewal in flight would make the marker again after the
			// delete: it ends first.
			close(srv.stop)
			<-srv.done
			ctx, cancel := context.WithTimeout(context.Background(), p.t.cliCleanup)
			_, _ = srv.l.lxcIdem(ctx, "network", "acl", "delete", srv.remote+":"+srv.l.marker(), "--project", "default")
			cancel()
		}
		srv.mu.Unlock()
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

// bridgeName is a slot bridge's name for a start's try: linux caps
// interface names at 15. each try names its own, since a create whose answer
// was lost can land after the failed try's delete.
func (l *lxd) bridgeName(instance string, try int) string {
	seed := instance + l.p.token
	if try > 1 {
		seed += "-" + strconv.Itoa(try)
	}
	sum := sha256.Sum256([]byte(seed))
	return "patsbr" + hex.EncodeToString(sum[:])[:8]
}

// createOwned runs a create of a network or acl (kind is its api path:
// networks, network-acls) in the default project. one that is already there
// is adopted when it carries this run's token, and an error otherwise.
func (l *lxd) createOwned(ctx context.Context, kind, name string, args ...string) error {
	_, err := l.lxc(ctx, args...)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	var got struct {
		Config map[string]string `json:"config"`
	}
	if qerr := l.query(ctx, "/1.0/"+kind+"/"+name+"?project=default", &got); qerr != nil {
		return fmt.Errorf("%w: %w (and reading it: %w)", errNotOurs, err, qerr)
	} else if got.Config["user.pats.run"] != l.p.token {
		return fmt.Errorf("%w: %w", errNotOurs, err)
	}
	return nil
}

// errNotOurs is a network or acl already there that is not known to be this
// run's: nothing of the run deletes it by name.
var errNotOurs = errors.New("not this run's")

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
// other; starts it; and pushes the workdir and mounts in. a try that failed
// on a lost link in one call is tried again from nothing, once all it made is
// confirmed gone (docs/spec/lxd-retry.md).
func (l *lxd) Start(ctx context.Context, spec Spec) (Session, error) {
	if err := l.prepareServer(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	backoff := l.p.t.lxdBackoff
	for try := 1; ; try++ {
		s, err := l.startOnce(ctx, spec, try)
		if err == nil {
			return s, nil
		}
		if !retryable(err) || try >= lxdTries || ctx.Err() != nil {
			// the start's own tries are the count a failure of one call gives.
			if pe := (*placeErr)(nil); try > 1 && errors.As(err, &pe) && pe.tries == 1 {
				pe.tries = try
			}
			bridge := s.bridge
			s.Close()
			if bridge != "" && errors.Is(err, ErrPlace) {
				// a create whose answer was lost may land after Close's delete.
				l.deferDelete("", bridge)
			}
			return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
		if rerr := s.removeForRetry(); rerr != nil {
			return nil, fmt.Errorf("%w: %w; the failed try could not be removed, so no other is made: %w", ErrLaunch, final(err), rerr)
		}
		d := backoff[min(try-1, len(backoff)-1)]
		spec.progress("start failed (%v), trying again in %s (try %d/%d)", err, d, try+1, lxdTries)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ErrLaunch, &placeErr{cause: errors.Join(err, ctx.Err()), tries: try, final: true})
		case <-l.p.clock.After(d):
		}
	}
}

// startOnce is one try at Start. on a failure it returns the session as far
// as it got, for the caller to close or remove.
func (l *lxd) startOnce(ctx context.Context, spec Spec, try int) (*lxdSession, error) {
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return &lxdSession{l: l}, fmt.Errorf("resolve workdir: %w", err)
	}
	if !spec.Egress.None {
		if err := l.o.machineCheck("lxd"); err != nil {
			return &lxdSession{l: l}, err
		}
	}
	s := &lxdSession{l: l, name: lxdName(abs), workdir: abs, sudo: spec.Sudo, try: try}
	fail := func(err error) (*lxdSession, error) { return s, err }

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
	args = append(args, limitFlags(l.s.Limits)...)
	for _, k := range sortedKeys(l.s.Config) {
		args = append(args, "-c", k+"="+l.s.Config[k])
	}
	switch n := l.s.Nested; {
	case n.Run == NestedNone:
	case l.o.VM:
		// a vm's kernel is its own: nothing to set. vms are probed once it
		// is up.
		if n.Run == NestedContainers {
			s.nested = &NestedResult{Asked: n.Run, Required: n.Required}
		}
	default:
		if n.Run == NestedVMs {
			return fail(errors.New("nested: vms needs vm: true: a container would get the host's kvm and vsock (docs/spec/nested.md)"))
		}
		if _, set := l.s.Config["security.nesting"]; !set {
			args = append(args, "-c", "security.nesting=true")
		}
		s.nested = &NestedResult{Asked: n.Run, Required: n.Required, Set: "security.nesting"}
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
		return fail(noRouteHint(err, s.bridge))
	}
	// before the binds' pushes: a sandbox that cannot costs a boot, no more.
	if l.o.VM && l.s.Nested.Run == NestedVMs {
		s.nested = s.probeNested(ctx, l.s.Nested)
		if ok := s.nested.OK(); l.s.Nested.Required && (ok == nil || !*ok) {
			return fail(&NestedError{Result: s.nested})
		}
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
		s.binds = append(s.binds, bind{host: mh, path: m.Container, ro: m.ReadOnly, masks: m.Masks})
	}
	for _, b := range s.binds {
		if err := s.pushBind(ctx, b); err != nil {
			return fail(err)
		}
	}
	// after the pushes: a link a bind brought in counts.
	if err := s.readOnlyHeld(ctx); err != nil {
		return fail(err)
	}
	// the agent owns what it was handed, a read-only bind excepted, which
	// stays root's; a root step may hand more over.
	if err := s.bindOwners(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

// limitFlags are an lxd init or launch's flags for lim.
func limitFlags(lim Limits) []string {
	var args []string
	if lim.CPU > 0 {
		args = append(args, "-c", "limits.cpu="+strconv.Itoa(lim.CPU))
	}
	if lim.Memory != "" {
		args = append(args, "-c", "limits.memory="+lim.Memory)
	}
	if lim.Disk != "" {
		args = append(args, "-d", "root,size="+lim.Disk)
	}
	return args
}

// deleteInstance removes an instance of the pats project for good, within
// ctx (the caller's bound). killing lxc does not stop the server's half of an
// init, so an instance whose init was cut short (a deadline, ctrl+c) can
// appear after a delete has already said it is not there; while an operation
// on it still runs, or the query for them fails, the delete is retried until
// it is over or ctx ends. the operations are read before each delete, not
// after: a create that was over before the delete cannot finish after it
// (docs/spec/fizz/lxd_delete.fizz). giving up, it says what it last saw.
func (l *lxd) deleteInstance(ctx context.Context, name string) error {
	start := l.p.clock.Now()
	var lastDel, lastQuery error
	var busyWith string
	for {
		busy, desc, qerr := l.instanceOps(ctx, name)
		_, err := l.lxc(ctx, "delete", l.ref(name), "--project", lxdProject, "--force")
		if lxdMissing(err) {
			err = nil
		}
		// a delete that lost the server is tried again, as a busy one is.
		if !busy && !errors.Is(err, ErrPlace) {
			return err
		}
		lastDel, lastQuery, busyWith = err, qerr, desc
		select {
		case <-ctx.Done():
			why := fmt.Sprintf("last delete: %v; last operations query: %v", orNone(lastDel), orNone(lastQuery))
			if busyWith != "" {
				why = "busy: " + busyWith
			}
			return fmt.Errorf("instance %s: a server operation on it, or the query for them, outlived %s (%s); pats clean removes it once this run has ended",
				name, l.p.clock.Now().Sub(start).Round(time.Second), why)
		case <-l.p.clock.After(l.p.t.deleteRetry):
		}
	}
}

// deleteGone is deleteInstance under instanceGone, a pair's cleanup's bound.
func (l *lxd) deleteGone(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), l.p.t.instanceGone)
	defer cancel()
	return l.deleteInstance(ctx, name)
}

// orNone is err's text, or "none".
func orNone(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

// instanceBusy says whether a running operation on the server names the
// instance. a failed query is busy: an idle reading would end the delete
// while a create may still land.
func (l *lxd) instanceBusy(ctx context.Context, name string) bool {
	busy, _, _ := l.instanceOps(ctx, name)
	return busy
}

// instanceOps is instanceBusy with what it saw: the description of the
// operation that named the instance, or the query's error.
func (l *lxd) instanceOps(ctx context.Context, name string) (busy bool, desc string, err error) {
	var ops struct {
		Running []lxdOperation `json:"running"`
	}
	if err := l.query(ctx, "/1.0/operations?project="+lxdProject+"&recursion=1", &ops); err != nil {
		return true, "", err
	}
	for _, op := range ops.Running {
		if op.names("/1.0/instances/" + name) {
			d := op.Description
			if d == "" {
				d = "an operation"
			}
			return true, d, nil
		}
	}
	return false, "", nil
}

// deferDelete puts what a cleanup could not remove on the process's list,
// for shutdown to try once more: the instance, then its bridge and acl
// (bridge is "" for none).
func (l *lxd) deferDelete(instance, bridge string) {
	x := &l.p.lxd
	x.deferMu.Lock()
	defer x.deferMu.Unlock()
	x.deferred = append(x.deferred, deferredDelete{l: l, instance: instance, bridge: bridge})
}

// deferredDelete is a pair's leftovers a cleanup gave up on.
type deferredDelete struct {
	l                *lxd
	instance, bridge string
}

// retryDeferred tries each deferred delete once more within ctx, in order:
// the instance, then the bridge and acl, which an instance still there holds.
func (x *lxdProcess) retryDeferred(ctx context.Context) {
	x.deferMu.Lock()
	ds := x.deferred
	x.deferred = nil
	x.deferMu.Unlock()
	for _, d := range ds {
		if d.instance != "" {
			_ = d.l.deleteInstance(ctx, d.instance)
		}
		if d.bridge != "" {
			_, _ = d.l.lxcIdem(ctx, "network", "delete", d.l.ref(d.bridge), "--project", "default")
			_, _ = d.l.lxcIdem(ctx, "network", "acl", "delete", d.l.ref(d.bridge), "--project", "default")
		}
	}
}

// lxdOperation is what an operation says of what it works on: lxd 5 lists
// it under resources, lxd 6 leaves those empty for a create and names the
// instance in metadata.entity_url.
type lxdOperation struct {
	Description string              `json:"description"`
	Resources   map[string][]string `json:"resources"`
	Metadata    map[string]any      `json:"metadata"`
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
