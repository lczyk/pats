package sandbox

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeServer answers the lxc cli the way an lxd 5.21 server does, for the
// argv the driver uses: it parses flags by name, holds what the driver makes
// (instances, networks, acls, images, projects, profiles, operations) and
// refuses what lxd refuses (a delete of something in use, a name taken, a
// reference to something missing). a subcommand, flag or query path it does
// not know fails the test, so its coverage is never assumed. probes are
// answered by name (probes.go), pushes and pulls go to a file tree per
// instance, and any other exec succeeds with no output unless agent says
// otherwise. see docs/spec/lxd-testing.md, seam 4.
type fakeServer struct {
	t *testing.T
	callLog

	mu       sync.Mutex
	remote   string
	arch     string
	version  string
	cpus     int
	pool     string
	used     uint64
	total    uint64
	projects map[string]bool
	profiles map[string]*fakeProfile // the pats project's
	insts    map[string]*fakeInst    // the pats project's
	nets     map[string]*fakeNet     // the default project's
	acls     map[string]*fakeACL     // the default project's
	images   map[string]*fakeImage   // the pats project's, by fingerprint
	bases    map[string]string       // refs on image servers -> fingerprint
	ops      []*fakeOp
	execs    map[*fakeExec]bool
	seq      int

	// probe answers.
	offline bool   // the online probe fails
	canary  string // the canary's output
	hostile string // the hostile probe's output

	// agent, when set, runs an exec no probe or file op claims: the agent's
	// command, a provisioning script. ctx ends when the exec is killed.
	agent func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error

	// slow names instances whose init or launch outlives a killed client:
	// the call blocks until its context ends, and the server finishes the
	// create that many calls later.
	slow map[string]int

	// before, when set, runs before each call is served, f.mu held: what
	// another client did to the server meanwhile.
	before func(args []string)

	faults faults
}

type fakeProfile struct {
	config  map[string]string
	devices map[string]map[string]string
}

type fakeInst struct {
	config   map[string]string
	devices  map[string]map[string]string
	profiles []string
	vm       bool
	running  bool
	files    map[string][]byte // regular files by absolute path
	dirs     map[string]bool
	users    map[string]int
}

type fakeNet struct {
	config map[string]string
	hwaddr string
}

type fakeACL struct {
	config map[string]string
	rules  []map[string]string
}

type fakeImage struct {
	props   map[string]string
	aliases []string
}

type fakeOp struct {
	inst string
	left int
	make func()
}

type fakeExec struct {
	inst, user string
	env        map[string]string
	kill       context.CancelFunc
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{
		t: t, remote: "srv", arch: "x86_64", version: "5.21.3", cpus: 8, pool: "default",
		used: 1 << 30, total: 100 << 30,
		projects: map[string]bool{"default": true},
		profiles: map[string]*fakeProfile{"default": {}},
		insts:    map[string]*fakeInst{},
		nets:     map[string]*fakeNet{"lxdbr0": {config: map[string]string{"ipv4.address": "10.1.0.1/24"}, hwaddr: "00:16:3e:00:00:01"}},
		acls:     map[string]*fakeACL{},
		images:   map[string]*fakeImage{},
		bases:    map[string]string{"img": fakeFP("img"), "ubuntu:26.04": fakeFP("ubuntu:26.04"), lxdProxyBase: fakeFP(lxdProxyBase)},
		execs:    map[*fakeExec]bool{},
		canary:   "egress-canary leak=0\n",
		hostile:  "egress-hostile leak=0\n",
		slow:     map[string]int{},
	}
	return f
}

func fakeFP(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// lxdOn is an lxd driver on f, with a process of its own.
func lxdOn(t *testing.T, f *fakeServer, o Options) *lxd {
	if o.Driver == "" {
		o.Driver = "lxd"
	}
	if o.Image == "" {
		o.Image = "img"
	}
	return newLXD(testProcess(t), runFunc(f.run), o)
}

func (f *fakeServer) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.record(args)
	f.mu.Lock()
	f.tickOps()
	if f.before != nil {
		f.before(args)
	}
	f.mu.Unlock()
	return f.faults.call(ctx, args, stderr, func(killed bool) error {
		return f.do(ctx, stdin, stdout, stderr, args, killed)
	})
}

// tickOps advances every running operation by one call; f.mu is held.
func (f *fakeServer) tickOps() {
	var left []*fakeOp
	for _, op := range f.ops {
		op.left--
		if op.left <= 0 {
			op.make()
		} else {
			left = append(left, op)
		}
	}
	f.ops = left
}

// fakeArgs is an argv parsed the way the cli parses it: flags by name.
type fakeArgs struct {
	pos   []string
	flags map[string][]string
	bools map[string]bool
	argv  []string // after --
}

func (a fakeArgs) flag(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (f *fakeServer) parse(args []string) (fakeArgs, error) {
	a := fakeArgs{flags: map[string][]string{}, bools: map[string]bool{}}
	valued := map[string]bool{"--project": true, "--storage": true, "-c": true, "-d": true, "-p": true, "-n": true,
		"--user": true, "--group": true, "--cwd": true, "--env": true, "--alias": true}
	boolean := map[string]bool{"--vm": true, "--force": true, "--force-noninteractive": true}
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case s == "--":
			a.argv = args[i+1:]
			return a, nil
		case valued[s]:
			if i+1 >= len(args) {
				return a, fmt.Errorf("flag %s needs a value", s)
			}
			a.flags[s] = append(a.flags[s], args[i+1])
			i++
		case boolean[s]:
			a.bools[s] = true
		case strings.HasPrefix(s, "-"):
			f.t.Errorf("fake lxd: unknown flag %s in lxc %s", s, strings.Join(args, " "))
			return a, fmt.Errorf("unknown flag %s", s)
		default:
			a.pos = append(a.pos, s)
		}
	}
	return a, nil
}

// lxcRef splits remote:name; an image server's ref keeps its remote.
func lxcRef(ref string) (remote, name string) {
	if i := strings.Index(ref, ":"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

// kv reads key=value positionals into a map.
func kv(pos []string) map[string]string {
	m := map[string]string{}
	for _, p := range pos {
		if k, v, ok := strings.Cut(p, "="); ok {
			m[k] = v
		}
	}
	return m
}

func lxcFail(stderr io.Writer, format string, a ...any) error {
	fmt.Fprintf(stderr, "Error: "+format+"\n", a...)
	return exitStatus(1)
}

// do serves one call; killed says the client dies once the server acted.
func (f *fakeServer) do(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string, killed bool) error {
	a, err := f.parse(args)
	if err != nil {
		return lxcFail(stderr, "%v", err)
	}
	p := a.pos
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	if at(0) == "exec" {
		return f.exec(ctx, stdin, stdout, stderr, a)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case at(0) == "remote" && at(1) == "get-default":
		fmt.Fprintln(stdout, f.remote)
		return nil
	case at(0) == "project" && at(1) == "show":
		_, name := lxcRef(at(2))
		if !f.projects[name] {
			return lxcFail(stderr, "Project not found")
		}
		fmt.Fprintf(stdout, "name: %s\n", name)
		return nil
	case at(0) == "project" && at(1) == "create":
		_, name := lxcRef(at(2))
		if f.projects[name] {
			return lxcFail(stderr, "Project %q already exists", name)
		}
		f.projects[name] = true
		return nil
	case at(0) == "profile" && at(1) == "device" && at(2) == "get":
		fmt.Fprintln(stdout, f.pool)
		return nil
	case at(0) == "network" && at(1) == "acl":
		return f.acl(stdout, stderr, a)
	case at(0) == "network":
		return f.network(stdout, stderr, a)
	case at(0) == "init" || at(0) == "launch":
		return f.create(ctx, stderr, a, killed)
	case at(0) == "config" && at(1) == "device" && at(2) == "add":
		return f.deviceAdd(stderr, a)
	case at(0) == "start":
		inst, err := f.inst(stderr, a, at(1))
		if err != nil {
			return err
		}
		for dn, d := range inst.devices {
			if d["type"] == "nic" && f.nets[d["network"]] == nil {
				return lxcFail(stderr, "Failed to start device %q: network %q not found", dn, d["network"])
			}
		}
		inst.running = true
		return nil
	case at(0) == "stop":
		inst, err := f.inst(stderr, a, at(1))
		if err != nil {
			return err
		}
		_, name := lxcRef(at(1))
		inst.running = false
		f.killExecs(name, "")
		return nil
	case at(0) == "delete":
		_, name := lxcRef(at(1))
		inst, err := f.inst(stderr, a, at(1))
		if err != nil {
			return err
		}
		if inst.running && !a.bools["--force"] {
			return lxcFail(stderr, "The instance is currently running, stop it first or use --force")
		}
		f.killExecs(name, "")
		delete(f.insts, name)
		return nil
	case at(0) == "query":
		return f.query(stdout, stderr, at(1))
	case at(0) == "image":
		return f.image(stdout, stderr, a)
	case at(0) == "publish":
		return f.publish(stderr, a)
	}
	f.t.Errorf("fake lxd: unknown command lxc %s", strings.Join(args, " "))
	return lxcFail(stderr, "unknown command")
}

func (f *fakeServer) needProject(stderr io.Writer, a fakeArgs, want string) error {
	got := a.flag("--project")
	if got == "" {
		got = "default"
	}
	if got != want {
		f.t.Errorf("fake lxd: %s in project %q, the driver keeps it in %q", strings.Join(a.pos, " "), got, want)
		return lxcFail(stderr, "wrong project %q", got)
	}
	if !f.projects[got] {
		return lxcFail(stderr, "Project not found")
	}
	return nil
}

func (f *fakeServer) inst(stderr io.Writer, a fakeArgs, ref string) (*fakeInst, error) {
	if err := f.needProject(stderr, a, lxdProject); err != nil {
		return nil, err
	}
	_, name := lxcRef(ref)
	inst := f.insts[name]
	if inst == nil {
		return nil, lxcFail(stderr, "Failed to fetch instance %q in project %q: Instance not found", name, lxdProject)
	}
	return inst, nil
}

func (f *fakeServer) acl(stdout, stderr io.Writer, a fakeArgs) error {
	if err := f.needProject(stderr, a, "default"); err != nil {
		return err
	}
	p := a.pos
	if p[2] == "rule" {
		_, name := lxcRef(p[4])
		acl := f.acls[name]
		if acl == nil {
			return lxcFail(stderr, "Network ACL not found")
		}
		rule := kv(p[6:])
		rule["direction"] = p[5]
		acl.rules = append(acl.rules, rule)
		return nil
	}
	_, name := lxcRef(p[3])
	acl := f.acls[name]
	switch p[2] {
	case "create":
		if acl != nil {
			return lxcFail(stderr, "The network ACL already exists")
		}
		f.acls[name] = &fakeACL{config: kv(p[4:])}
		return nil
	case "show":
		if acl == nil {
			return lxcFail(stderr, "Network ACL not found")
		}
		fmt.Fprintf(stdout, "name: %s\n", name)
		return nil
	case "set":
		if acl == nil {
			return lxcFail(stderr, "Network ACL not found")
		}
		for k, v := range kv(p[4:]) {
			acl.config[k] = v
		}
		return nil
	case "delete":
		if acl == nil {
			return lxcFail(stderr, "Network ACL not found")
		}
		for nn, n := range f.nets {
			for _, x := range strings.Split(n.config["security.acls"], ",") {
				if x == name {
					return lxcFail(stderr, "Cannot delete an ACL that is in use by network %q", nn)
				}
			}
		}
		delete(f.acls, name)
		return nil
	}
	f.t.Errorf("fake lxd: unknown command lxc %s", strings.Join(p, " "))
	return lxcFail(stderr, "unknown command")
}

func (f *fakeServer) network(stdout, stderr io.Writer, a fakeArgs) error {
	if err := f.needProject(stderr, a, "default"); err != nil {
		return err
	}
	p := a.pos
	_, name := lxcRef(p[2])
	n := f.nets[name]
	switch p[1] {
	case "create":
		if n != nil {
			return lxcFail(stderr, "The network already exists")
		}
		cfg := kv(p[3:])
		for _, x := range strings.Split(cfg["security.acls"], ",") {
			if x != "" && f.acls[x] == nil {
				return lxcFail(stderr, "Failed loading network ACL %q: Network ACL not found", x)
			}
		}
		f.seq++
		f.nets[name] = &fakeNet{config: cfg, hwaddr: fmt.Sprintf("00:16:3e:00:%02x:%02x", f.seq/256, f.seq%256)}
		return nil
	case "show":
		if n == nil {
			return lxcFail(stderr, "Network not found")
		}
		fmt.Fprintf(stdout, "name: %s\n", name)
		return nil
	case "delete":
		if n == nil {
			return lxcFail(stderr, "Network not found")
		}
		for iname, inst := range f.insts {
			for _, d := range inst.devices {
				if d["type"] == "nic" && d["network"] == name {
					return lxcFail(stderr, "The network is currently in use by instance %q", iname)
				}
			}
		}
		delete(f.nets, name)
		return nil
	}
	f.t.Errorf("fake lxd: unknown command lxc %s", strings.Join(p, " "))
	return lxcFail(stderr, "unknown command")
}

// resolveImage finds what init or launch makes an instance from.
func (f *fakeServer) resolveImage(ref string) bool {
	if _, ok := f.bases[ref]; ok {
		return true
	}
	r, name := lxcRef(ref)
	if r != "" && r != f.remote {
		return false
	}
	return f.lookupImage(name) != ""
}

// lookupImage is the fingerprint an alias or fingerprint names in the pats
// project, "" when none.
func (f *fakeServer) lookupImage(name string) string {
	if _, ok := f.images[name]; ok {
		return name
	}
	for fp, im := range f.images {
		for _, al := range im.aliases {
			if al == name {
				return fp
			}
		}
	}
	return ""
}

func (f *fakeServer) create(ctx context.Context, stderr io.Writer, a fakeArgs, killed bool) error {
	if err := f.needProject(stderr, a, lxdProject); err != nil {
		return err
	}
	p := a.pos
	if len(p) != 3 {
		f.t.Errorf("fake lxd: lxc %s: want an image and a name", strings.Join(p, " "))
		return lxcFail(stderr, "bad args")
	}
	launch := p[0] == "launch"
	r, name := lxcRef(p[2])
	if r != f.remote {
		f.t.Errorf("fake lxd: instance %s not on the pinned remote", p[2])
	}
	if f.insts[name] != nil {
		return lxcFail(stderr, "Failed creating instance record: Instance %q already exists", name)
	}
	if !f.resolveImage(p[1]) {
		return lxcFail(stderr, "Failed getting image: Image %q not found", p[1])
	}
	if s := a.flag("--storage"); s != f.pool {
		return lxcFail(stderr, "Storage pool %q not found", s)
	}
	profiles := a.flags["-p"]
	for _, pr := range profiles {
		if f.profiles[pr] == nil {
			return lxcFail(stderr, "Profile %q not found", pr)
		}
	}
	if len(profiles) == 0 {
		profiles = []string{"default"}
	}
	inst := &fakeInst{config: kv(a.flags["-c"]), devices: map[string]map[string]string{}, profiles: profiles,
		vm: a.bools["--vm"], files: map[string][]byte{caBundlePath: []byte("system roots\n")}, dirs: map[string]bool{"/": true},
		users: map[string]int{"root": 0}}
	for _, d := range a.flags["-d"] {
		dev, rest, _ := strings.Cut(d, ",")
		inst.devices[dev] = kv(strings.Split(rest, ","))
	}
	if launch {
		net := a.flag("-n")
		if f.nets[net] == nil {
			return lxcFail(stderr, "Network %q not found", net)
		}
		inst.devices["eth0"] = map[string]string{"type": "nic", "network": net, "name": "eth0"}
		inst.running = true
	}
	mk := func() { f.insts[name] = inst }
	if killed {
		// the client is gone; the server's half of the create goes on.
		f.ops = append(f.ops, &fakeOp{inst: name, left: 1, make: mk})
		return exitStatus(-1)
	}
	if calls, ok := f.slow[name]; ok {
		delete(f.slow, name)
		f.ops = append(f.ops, &fakeOp{inst: name, left: calls, make: mk})
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return exitStatus(-1)
	}
	mk()
	return nil
}

func (f *fakeServer) deviceAdd(stderr io.Writer, a fakeArgs) error {
	inst, err := f.inst(stderr, a, a.pos[3])
	if err != nil {
		return err
	}
	dev := a.pos[4]
	if inst.devices[dev] != nil {
		return lxcFail(stderr, "The device already exists")
	}
	d := kv(a.pos[6:])
	d["type"] = a.pos[5]
	if d["type"] == "nic" && f.nets[d["network"]] == nil {
		return lxcFail(stderr, "Invalid devices: Device validation failed for %q: Failed loading network %q: Network not found", dev, d["network"])
	}
	inst.devices[dev] = d
	return nil
}

func (f *fakeServer) image(stdout, stderr io.Writer, a fakeArgs) error {
	p := a.pos
	switch {
	case p[1] == "info":
		ref := p[2]
		if fp, ok := f.bases[ref]; ok {
			fmt.Fprintf(stdout, "Fingerprint: %s\nSize: 100.00MiB\n", fp)
			return nil
		}
		if err := f.needProject(stderr, a, lxdProject); err != nil {
			return err
		}
		_, name := lxcRef(ref)
		fp := f.lookupImage(name)
		if fp == "" {
			return lxcFail(stderr, "not found")
		}
		fmt.Fprintf(stdout, "Fingerprint: %s\nSize: 100.00MiB\n", fp)
		return nil
	case p[1] == "alias" && p[2] == "create":
		if err := f.needProject(stderr, a, lxdProject); err != nil {
			return err
		}
		_, name := lxcRef(p[3])
		if f.lookupImage(name) != "" {
			return lxcFail(stderr, "Alias already exists")
		}
		im := f.images[p[4]]
		if im == nil {
			return lxcFail(stderr, "Image not found")
		}
		im.aliases = append(im.aliases, name)
		return nil
	case p[1] == "alias" && p[2] == "delete":
		if err := f.needProject(stderr, a, lxdProject); err != nil {
			return err
		}
		_, name := lxcRef(p[3])
		for _, im := range f.images {
			for i, al := range im.aliases {
				if al == name {
					im.aliases = append(im.aliases[:i], im.aliases[i+1:]...)
					return nil
				}
			}
		}
		return lxcFail(stderr, "Alias not found")
	case p[1] == "delete":
		if err := f.needProject(stderr, a, lxdProject); err != nil {
			return err
		}
		_, name := lxcRef(p[2])
		fp := f.lookupImage(name)
		if fp == "" {
			return lxcFail(stderr, "Image not found")
		}
		delete(f.images, fp)
		return nil
	}
	f.t.Errorf("fake lxd: unknown command lxc %s", strings.Join(p, " "))
	return lxcFail(stderr, "unknown command")
}

func (f *fakeServer) publish(stderr io.Writer, a fakeArgs) error {
	inst, err := f.inst(stderr, a, a.pos[1])
	if err != nil {
		return err
	}
	if a.pos[2] != f.remote+":" {
		f.t.Errorf("fake lxd: publish to %q, not the pinned remote", a.pos[2])
	}
	if inst.running {
		return lxcFail(stderr, "The instance is currently running. Use --force to have it stopped and restarted")
	}
	alias := a.flag("--alias")
	if alias != "" && f.lookupImage(alias) != "" {
		return lxcFail(stderr, "Alias already exists")
	}
	f.seq++
	fp := fakeFP(fmt.Sprintf("%s/%d", a.pos[1], f.seq))
	im := &fakeImage{props: kv(a.pos[3:])}
	if alias != "" {
		im.aliases = []string{alias}
	}
	f.images[fp] = im
	return nil
}

func (f *fakeServer) query(stdout, stderr io.Writer, raw string) error {
	r, rest := lxcRef(raw)
	if !strings.HasPrefix(raw, "/") && r != f.remote {
		f.t.Errorf("fake lxd: query %q not on the pinned remote", raw)
	}
	if strings.HasPrefix(raw, "/") {
		rest = raw
	}
	u, err := url.Parse(rest)
	if err != nil {
		return lxcFail(stderr, "%v", err)
	}
	q := u.Query()
	out := func(v any) error {
		b, _ := json.Marshal(v)
		stdout.Write(b)
		return nil
	}
	project := func() error {
		if !f.projects[q.Get("project")] {
			return lxcFail(stderr, "Project not found")
		}
		return nil
	}
	switch p := u.Path; {
	case p == "/1.0":
		return out(map[string]any{"environment": map[string]any{"architectures": []string{f.arch}, "server_version": f.version}})
	case p == "/1.0/resources":
		return out(map[string]any{"cpu": map[string]any{"total": f.cpus}})
	case p == "/1.0/instances" && q.Get("recursion") == "1":
		if err := project(); err != nil {
			return err
		}
		list := []map[string]any{}
		for _, name := range sortedNames(f.insts) {
			list = append(list, map[string]any{"name": name, "config": f.insts[name].config})
		}
		return out(list)
	case strings.HasPrefix(p, "/1.0/instances/"):
		if err := project(); err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "/1.0/instances/")
		inst := f.insts[name]
		if inst == nil {
			return lxcFail(stderr, "Instance not found")
		}
		cfg, devs := map[string]string{}, map[string]map[string]string{}
		for _, pr := range inst.profiles {
			for k, v := range f.profiles[pr].config {
				cfg[k] = v
			}
			for k, v := range f.profiles[pr].devices {
				devs[k] = v
			}
		}
		for k, v := range inst.config {
			cfg[k] = v
		}
		for k, v := range inst.devices {
			devs[k] = v
		}
		return out(map[string]any{"name": name, "expanded_config": cfg, "expanded_devices": devs})
	case p == "/1.0/networks" && q.Get("recursion") == "1":
		list := []map[string]any{}
		for _, name := range sortedNames(f.nets) {
			list = append(list, map[string]any{"name": name, "config": f.nets[name].config})
		}
		return out(list)
	case strings.HasPrefix(p, "/1.0/networks/") && strings.HasSuffix(p, "/state"):
		n := f.nets[strings.TrimSuffix(strings.TrimPrefix(p, "/1.0/networks/"), "/state")]
		if n == nil {
			return lxcFail(stderr, "Network not found")
		}
		return out(map[string]any{"hwaddr": n.hwaddr})
	case p == "/1.0/network-acls" && q.Get("recursion") == "1":
		list := []map[string]any{}
		for _, name := range sortedNames(f.acls) {
			list = append(list, map[string]any{"name": name, "config": f.acls[name].config})
		}
		return out(list)
	case p == "/1.0/images" && q.Get("recursion") == "1":
		if err := project(); err != nil {
			return err
		}
		list := []map[string]any{}
		for _, fp := range sortedNames(f.images) {
			im := f.images[fp]
			var aliases []map[string]string
			for _, al := range im.aliases {
				aliases = append(aliases, map[string]string{"name": al})
			}
			list = append(list, map[string]any{"fingerprint": fp, "properties": im.props, "aliases": aliases})
		}
		return out(list)
	case p == "/1.0/operations" && q.Get("recursion") == "1":
		running := []map[string]any{}
		for _, op := range f.ops {
			running = append(running, map[string]any{"resources": map[string][]string{
				"instances": {"/1.0/instances/" + op.inst + "?project=" + lxdProject}}})
		}
		return out(map[string]any{"running": running})
	case strings.HasPrefix(p, "/1.0/storage-pools/") && strings.HasSuffix(p, "/resources"):
		if strings.TrimSuffix(strings.TrimPrefix(p, "/1.0/storage-pools/"), "/resources") != f.pool {
			return lxcFail(stderr, "Storage pool not found")
		}
		return out(map[string]any{"space": map[string]any{"used": f.used, "total": f.total}})
	}
	f.t.Errorf("fake lxd: unknown query path %s", raw)
	return lxcFail(stderr, "not found")
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// killExecs ends the execs running in inst, all or only user's; f.mu is
// held. they are gone from the server at once, as a killed process is.
func (f *fakeServer) killExecs(inst, user string) {
	for x := range f.execs {
		if x.inst == inst && (user == "" || x.user == user) {
			x.kill()
			delete(f.execs, x)
		}
	}
}

// exec runs argv in an instance: a probe by name, a push or pull, a file
// read, a pkill, the egress proxy, or the agent.
func (f *fakeServer) exec(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, a fakeArgs) error {
	f.mu.Lock()
	inst, err := f.inst(stderr, a, a.pos[1])
	if err == nil && !inst.running {
		err = lxcFail(stderr, "Instance is not running")
	}
	if err == nil && (!a.bools["--force-noninteractive"] || a.flag("--user") == "" || a.flag("--cwd") == "" || len(a.argv) == 0) {
		f.t.Errorf("fake lxd: exec without --force-noninteractive, --user, --cwd or argv: %v", a)
	}
	if err != nil {
		f.mu.Unlock()
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		return err
	}
	_, name := lxcRef(a.pos[1])
	xctx, kill := context.WithCancel(ctx)
	defer kill()
	x := &fakeExec{inst: name, user: a.flag("--user"), env: kv(a.flags["--env"]), kill: kill}
	f.execs[x] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.execs, x)
		f.mu.Unlock()
	}()

	argv := a.argv
	if len(argv) >= 4 && (argv[0] == "sh" || argv[0] == "bash") && argv[1] == "-c" {
		return f.probe(xctx, inst, name, argv[3], argv[4:], argv, stdin, stdout, stderr)
	}
	switch {
	case argv[0] == "tar" && len(argv) == 5 && argv[1] == "-c" && argv[2] == "-C":
		f.mu.Lock()
		defer f.mu.Unlock()
		return writeFakeTar(inst, argv[3], stdout)
	case argv[0] == "cat" && len(argv) == 2:
		f.mu.Lock()
		b, ok := inst.files[argv[1]]
		f.mu.Unlock()
		if !ok {
			fmt.Fprintf(stderr, "cat: %s: No such file or directory\n", argv[1])
			return exitStatus(1)
		}
		stdout.Write(b)
		return nil
	case argv[0] == "pkill" && len(argv) == 4 && argv[1] == "-KILL" && argv[2] == "-u":
		f.mu.Lock()
		f.killExecs(name, argv[3])
		f.mu.Unlock()
		return nil
	case argv[0] == "/usr/local/bin/pats" && len(argv) == 3 && argv[1] == "egress" && argv[2] == "run":
		fmt.Fprintf(stderr, "listening %s\n", x.env["PATS_EGRESS_LISTEN"])
		<-xctx.Done()
		return exitStatus(-1)
	}
	return f.runAgent(xctx, name, argv, stdout, stderr)
}

func (f *fakeServer) runAgent(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
	if f.agent == nil {
		return nil
	}
	if err := f.agent(ctx, inst, argv, stdout, stderr); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return exitStatus(137) // killed
	}
	return nil
}

func (f *fakeServer) probe(ctx context.Context, inst *fakeInst, name, probeName string, args, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch probeName {
	case probeReady.name:
		return nil
	case probeRoute.name:
		for _, d := range inst.devices {
			if n := f.nets[d["network"]]; d["type"] == "nic" && n != nil && n.config["ipv4.address"] != "none" {
				return nil
			}
		}
		return exitStatus(1)
	case probeOnline.name:
		if f.offline {
			return exitStatus(1)
		}
		return nil
	case probeCanary.name:
		io.WriteString(stdout, f.canary)
		return nil
	case probeReach.name:
		ok := 0
		if f.reaches(inst, args[0], args[1]) {
			ok = 1
		}
		fmt.Fprintf(stdout, "egress-reach ok=%d\n", ok)
		return nil
	case probeHostile.name:
		io.WriteString(stdout, f.hostile)
		return nil
	case probeAgentUser.name, probeProxyUser.name:
		user := lxdAgentUser
		if probeName == probeProxyUser.name {
			user = lxdProxyUser
		}
		uid, ok := inst.users[user]
		if !ok {
			if probeName == probeProxyUser.name {
				uid = 999 // the proxy image's provisioning made it
			} else {
				uid = 1000 + len(inst.users)
			}
			inst.users[user] = uid
		}
		fmt.Fprintln(stdout, uid)
		return nil
	case "push":
		f.mu.Unlock()
		err := readFakeTar(stdin, inst, args[0], args[1], &f.mu)
		f.mu.Lock()
		if err != nil {
			fmt.Fprintf(stderr, "tar: %v\n", err)
			return exitStatus(2)
		}
		return nil
	case "handover", "provision":
		if probeName == "provision" {
			f.mu.Unlock()
			err := f.runAgent(ctx, name, argv, stdout, stderr)
			f.mu.Lock()
			return err
		}
		return nil
	}
	// a script whose output nobody reads: sysctl, ip addr, sudoers, the
	// build epilogue, cloud-init.
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	return nil
}

// reaches says whether inst's bridge lets it reach ip:port, and something
// listens there: the acl allows it and a proxy listens on it. f.mu is held.
func (f *fakeServer) reaches(inst *fakeInst, ip, port string) bool {
	listening := false
	for x := range f.execs {
		if x.env["PATS_EGRESS_LISTEN"] == ip+":"+port {
			listening = true
		}
	}
	if !listening {
		return false
	}
	for _, d := range inst.devices {
		n := f.nets[d["network"]]
		if d["type"] != "nic" || n == nil {
			continue
		}
		acl := f.acls[n.config["security.acls"]]
		if acl == nil {
			continue
		}
		for _, r := range acl.rules {
			if r["action"] == "allow" && r["destination"] == ip+"/32" && r["destination_port"] == port {
				return true
			}
		}
	}
	return false
}

// readFakeTar extracts a pushed tar into dest; a single file lands at want.
func readFakeTar(r io.Reader, inst *fakeInst, dest, want string, mu *sync.Mutex) error {
	tr := tar.NewReader(r)
	var files []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p := path.Join(dest, h.Name)
		var b []byte
		if h.Typeflag == tar.TypeReg {
			if b, err = io.ReadAll(tr); err != nil {
				return err
			}
		}
		mu.Lock()
		if h.Typeflag == tar.TypeDir {
			inst.dirs[p] = true
		} else {
			inst.files[p] = b
			files = append(files, p)
		}
		mu.Unlock()
	}
	_, _ = io.Copy(io.Discard, r)
	mu.Lock()
	defer mu.Unlock()
	inst.dirs[dest] = true
	if dest != want && len(files) == 1 && files[0] != want {
		inst.files[want] = inst.files[files[0]]
		delete(inst.files, files[0])
	}
	return nil
}

// writeFakeTar writes what inst holds under dir as a tar; f.mu is held.
func writeFakeTar(inst *fakeInst, dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, p := range sortedNames(inst.files) {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		b := inst.files[p]
		if err := tw.WriteHeader(&tar.Header{Name: "./" + strings.TrimPrefix(p, prefix), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

// leftovers is what f holds of token's run beyond the caches: its
// instances, networks, acls (the marker included), images not cached under a
// pats-img- alias, operations and execs still running.
func (f *fakeServer) leftovers(token string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	mine := func(cfg map[string]string, name string) bool {
		return cfg["user.pats.run"] == token || strings.Contains(name, token)
	}
	for name, inst := range f.insts {
		if mine(inst.config, name) {
			out = append(out, "instance "+name)
		}
	}
	for name, n := range f.nets {
		if mine(n.config, name) {
			out = append(out, "network "+name)
		}
	}
	for name, a := range f.acls {
		if mine(a.config, name) {
			out = append(out, "acl "+name)
		}
	}
	for fp, im := range f.images {
		cached := false
		for _, al := range im.aliases {
			if strings.HasPrefix(al, "pats-img-") && !strings.HasSuffix(al, "-"+token) {
				cached = true
			}
		}
		if !cached && mine(im.props, strings.Join(im.aliases, ",")) {
			out = append(out, "image "+fp[:12]+" "+strings.Join(im.aliases, ","))
		}
		for _, al := range im.aliases {
			if cached && strings.HasSuffix(al, "-"+token) {
				out = append(out, "alias "+al+" on a cached image")
			}
		}
	}
	for _, op := range f.ops {
		out = append(out, "operation on "+op.inst)
	}
	for x := range f.execs {
		out = append(out, "exec in "+x.inst)
	}
	sort.Strings(out)
	return out
}

// put stores a file in an instance, for a test to pull.
func (f *fakeServer) put(inst, p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insts[inst].files[p] = []byte(content)
}

// file reads a file out of an instance.
func (f *fakeServer) file(inst, p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.insts[inst]
	if i == nil {
		return "", false
	}
	b, ok := i.files[p]
	return string(b), ok
}

func (f *fakeServer) has(kind, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "instance":
		return f.insts[name] != nil
	case "network":
		return f.nets[name] != nil
	case "acl":
		return f.acls[name] != nil
	case "image":
		return f.lookupImage(name) != ""
	}
	return false
}
