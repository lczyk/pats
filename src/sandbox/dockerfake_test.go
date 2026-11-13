package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon answers the docker (or podman) cli the way the daemon does, for
// the argv the container driver uses: flags parsed by name up to the image
// or container, containers, networks and their addresses held as state, the
// daemon's refusals given (a name or subnet taken, a network in use, no such
// container), the cidfile written as the cli writes it, docker cp's rules
// for where a copy lands, probes answered by name, and `logs -f` running
// until its container goes. a subcommand or flag it does not know fails the
// test. the docker counterpart of fakeServer (docs/spec/sandbox-seams.md).
type fakeDaemon struct {
	t *testing.T
	callLog

	mu     sync.Mutex
	driver string
	ctrs   map[string]*fakeCtr // by id
	nets   map[string]*fakeDNet
	execs  map[*fakeExec]bool
	seq    int

	images   map[string]*fakeDImage       // by id
	registry map[string]fakeRegistryImage // what a pull of a ref brings
	conns    []map[string]any             // podman's connections
	daemonID string
	cpus     int
	pulls    int
	builds   int

	canary     string // the canary's output
	remapsInfo string // what info says of uid mapping
	rootDir    string // what info says of the daemon's root dir
	// upstream is the machine's proxy, ip:port, reached from the default
	// network.
	upstream     string
	upstreamDown bool // a proxy behind an upstream exits, unable to reach it
	// agent, when set, runs an exec the fake does not claim (the agent's
	// command, a root step). ctx ends when the container is killed.
	agent func(ctx context.Context, ctr string, argv []string, stdout, stderr io.Writer) error

	faults faults
}

type fakeCtr struct {
	id, name, image string
	labels          map[string]string
	nets            map[string]netip.Addr
	env             map[string]string
	binds           []fakeBind
	files           map[string][]byte // outside the binds
	dirs            map[string]bool
	rm              bool // --rm: gone once it stops
	created         time.Time
	gone            chan struct{} // closed when it is removed
}

type fakeBind struct {
	host, path string
	ro         bool
}

type fakeDNet struct {
	subnet   netip.Prefix
	internal bool
	labels   map[string]string
	created  time.Time
}

func newFakeDaemon(t *testing.T, driver string) *fakeDaemon {
	def := "bridge"
	if driver == "podman" {
		def = "podman"
	}
	return &fakeDaemon{t: t, driver: driver, ctrs: map[string]*fakeCtr{}, execs: map[*fakeExec]bool{},
		nets:   map[string]*fakeDNet{def: {subnet: netip.MustParsePrefix("172.17.0.0/16")}},
		canary: "egress-canary leak=0\n",
		images: map[string]*fakeDImage{}, daemonID: "DAEMON:" + driver, cpus: 6,
		registry: map[string]fakeRegistryImage{
			"img":            {fakeImageID("img"), "sha256:d-img"},
			"ubuntu:26.04":   {fakeImageID("ubuntu:26.04"), "sha256:d-ubuntu"},
			"python:3":       {fakeImageID("python:3"), "sha256:d-python"},
			"other/python:3": {fakeImageID("python:3"), "sha256:d-other"},
		}}
}

// containerOn is a docker driver on f, with a process of its own.
func containerOn(t *testing.T, f *fakeDaemon) *container {
	return newContainer(testProcess(t), runFunc(f.run), Options{Driver: f.driver}).on(SandboxOptions{Image: "img"})
}

func (f *fakeDaemon) run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.record(args)
	return f.faults.call(ctx, args, stderr, func(killed bool) error {
		return f.do(ctx, stdin, stdout, stderr, args)
	})
}

func dockerFail(stderr io.Writer, format string, a ...any) error {
	fmt.Fprintf(stderr, "Error response from daemon: "+format+"\n", a...)
	return exitStatus(125)
}

// dockerArgs parses flags by name up to the first positional, as the cli
// does for run and exec: what follows is the image or container and its
// command, never flags.
func (f *fakeDaemon) dockerArgs(args []string, valued, boolean map[string]bool) (flags map[string][]string, bools map[string]bool, rest []string, ok bool) {
	flags, bools = map[string][]string{}, map[string]bool{}
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case valued[s]:
			if i+1 >= len(args) {
				return nil, nil, nil, false
			}
			flags[s] = append(flags[s], args[i+1])
			i++
		case boolean[s]:
			bools[s] = true
		case strings.HasPrefix(s, "-"):
			f.t.Errorf("fake %s: unknown flag %s in %s", f.driver, s, strings.Join(args, " "))
			return nil, nil, nil, false
		default:
			return flags, bools, args[i:], true
		}
	}
	return flags, bools, nil, true
}

func (f *fakeDaemon) do(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) error {
	switch {
	case args[0] == "run":
		return f.runCtr(ctx, stdout, stderr, args[1:])
	case args[0] == "exec":
		return f.exec(ctx, stdin, stdout, stderr, args[1:])
	case args[0] == "logs" && len(args) == 3 && args[1] == "-f":
		return f.logs(ctx, stderr, args[2])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case args[0] == "rm" && len(args) == 3 && args[1] == "-f":
		c := f.lookup(args[2])
		if c == nil {
			return dockerFail(stderr, "No such container: %s", args[2])
		}
		f.remove(c)
		return nil
	case args[0] == "kill" && len(args) == 2:
		c := f.lookup(args[1])
		if c == nil {
			return dockerFail(stderr, "No such container: %s", args[1])
		}
		if c.rm {
			f.remove(c)
			return nil
		}
		for x := range f.execs { // stopped: what ran in it ends
			if x.inst == c.id {
				x.kill()
				delete(f.execs, x)
			}
		}
		return nil
	case args[0] == "cp" && len(args) == 3:
		return f.cp(stderr, args[1], args[2])
	case args[0] == "network" && len(args) > 1 && args[1] == "create":
		return f.netCreate(stderr, args[2:])
	case args[0] == "network" && len(args) == 3 && args[1] == "rm":
		n := f.nets[args[2]]
		if n == nil {
			return dockerFail(stderr, "network %s not found", args[2])
		}
		for _, c := range f.ctrs {
			if _, ok := c.nets[args[2]]; ok {
				return dockerFail(stderr, "error while removing network: network %s id x has active endpoints", args[2])
			}
		}
		delete(f.nets, args[2])
		return nil
	case args[0] == "network" && len(args) == 4 && args[1] == "connect":
		n, c := f.nets[args[2]], f.lookup(args[3])
		if n == nil || c == nil {
			return dockerFail(stderr, "No such network or container: %s %s", args[2], args[3])
		}
		c.nets[args[2]] = netip.Addr{}
		return nil
	}
	if handled, err := f.imageCmd(stdout, stderr, args); handled {
		return err
	}
	f.t.Errorf("fake %s: unknown command %s", f.driver, strings.Join(args, " "))
	return dockerFail(stderr, "unknown command")
}

// lookup finds a container by id or name; f.mu is held.
func (f *fakeDaemon) lookup(ref string) *fakeCtr {
	if c := f.ctrs[ref]; c != nil {
		return c
	}
	for _, c := range f.ctrs {
		if c.name != "" && c.name == ref {
			return c
		}
	}
	return nil
}

// remove takes c away, ending what runs in it; f.mu is held.
func (f *fakeDaemon) remove(c *fakeCtr) {
	delete(f.ctrs, c.id)
	close(c.gone)
	for x := range f.execs {
		if x.inst == c.id {
			x.kill()
			delete(f.execs, x)
		}
	}
}

var runValued = map[string]bool{"--cap-drop": true, "--security-opt": true, "-w": true, "-v": true, "--name": true,
	"--label": true, "--network": true, "--dns": true, "--ip": true, "--sysctl": true, "-e": true, "--cpus": true,
	"--memory": true, "--cidfile": true, "--entrypoint": true, "--user": true, "--mount": true}
var runBool = map[string]bool{"-d": true, "--rm": true, "--init": true, "--http-proxy=false": true}

func (f *fakeDaemon) runCtr(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	flags, bools, rest, ok := f.dockerArgs(args, runValued, runBool)
	if !ok || len(rest) == 0 {
		return dockerFail(stderr, "bad run: %s", strings.Join(args, " "))
	}
	last := func(k string) string {
		if v := flags[k]; len(v) > 0 {
			return v[len(v)-1]
		}
		return ""
	}
	// every container pats makes is a run's, or clean never finds it.
	if !hasLabel(flags["--label"], "pats.run=") || !hasLabel(flags["--label"], "pats.owner=") {
		f.t.Errorf("fake %s: a container without the run labels: run %s", f.driver, strings.Join(args, " "))
	}
	f.mu.Lock()
	net := last("--network")
	if net != "" && net != "none" && f.nets[net] == nil {
		f.mu.Unlock()
		return dockerFail(stderr, "network %s not found", net)
	}
	image, argv := rest[0], rest[1:]
	if !bools["-d"] {
		// a run to completion: a probe, or a file out of the image.
		f.mu.Unlock()
		return f.oneShot(stdout, stderr, last("--entrypoint"), net, argv)
	}
	defer f.mu.Unlock()
	if name := last("--name"); name != "" && f.lookup(name) != nil {
		return dockerFail(stderr, "Conflict. The container name %q is already in use", "/"+name)
	}
	f.seq++
	c := &fakeCtr{id: fmt.Sprintf("%064x", f.seq), name: last("--name"), image: image, labels: map[string]string{}, created: time.Now(),
		nets: map[string]netip.Addr{}, env: map[string]string{}, files: map[string][]byte{}, dirs: map[string]bool{"/": true},
		rm: bools["--rm"], gone: make(chan struct{})}
	for _, l := range flags["--label"] {
		k, v, _ := strings.Cut(l, "=")
		c.labels[k] = v
	}
	for _, e := range flags["-e"] {
		k, v, _ := strings.Cut(e, "=")
		c.env[k] = v
	}
	for _, v := range flags["-v"] {
		parts := strings.Split(v, ":")
		if len(parts) < 2 {
			return dockerFail(stderr, "invalid volume %q", v)
		}
		c.binds = append(c.binds, fakeBind{host: parts[0], path: parts[1], ro: len(parts) > 2 && parts[2] == "ro"})
	}
	if net != "" && net != "none" {
		ip := netip.Addr{}
		if s := last("--ip"); s != "" {
			a, err := netip.ParseAddr(s)
			if err != nil || !f.nets[net].subnet.Contains(a) {
				return dockerFail(stderr, "Invalid address %s: It does not belong to any of this network's subnets", s)
			}
			for _, o := range f.ctrs {
				if o.nets[net] == a {
					return dockerFail(stderr, "Address already in use")
				}
			}
			ip = a
		}
		c.nets[net] = ip
	}
	if cid := last("--cidfile"); cid != "" {
		if _, err := os.Stat(cid); err == nil {
			return dockerFail(stderr, "container ID file found, make sure the other container isn't running or delete %s", cid)
		}
		if err := os.WriteFile(cid, []byte(c.id), 0o644); err != nil {
			return dockerFail(stderr, "%v", err)
		}
	}
	f.ctrs[c.id] = c
	fmt.Fprintln(stdout, c.id)
	return nil
}

// oneShot runs a container to completion: the probes by name, or cat.
func (f *fakeDaemon) oneShot(stdout, stderr io.Writer, entrypoint, net string, argv []string) error {
	switch {
	case entrypoint == "cat" && len(argv) == 1 && argv[0] == caBundlePath:
		io.WriteString(stdout, "system roots\n")
		return nil
	case len(argv) >= 3 && argv[0] == "-c":
		switch argv[2] {
		case probeCanary.name:
			f.mu.Lock()
			io.WriteString(stdout, f.canary)
			f.mu.Unlock()
			return nil
		case probeReach.name:
			ok := 0
			if f.listening(net, argv[3], argv[4]) || (net == engines[f.driver].defaultNetwork && f.upstream == argv[3]+":"+argv[4]) {
				ok = 1
			}
			fmt.Fprintf(stdout, "egress-reach ok=%d\n", ok)
			return nil
		}
	}
	f.t.Errorf("fake %s: unknown one-shot run %s %v", f.driver, entrypoint, argv)
	return exitStatus(127)
}

// listening says whether a container on net serves ip:port.
func (f *fakeDaemon) listening(net, ip, port string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ctrs {
		if a, ok := c.nets[net]; ok && a.String() == ip && c.env["PATS_EGRESS_LISTEN"] == ip+":"+port {
			return true
		}
	}
	return false
}

func (f *fakeDaemon) netCreate(stderr io.Writer, args []string) error {
	valued := map[string]bool{"--label": true, "-o": true, "--subnet": true}
	boolean := map[string]bool{"--internal": true, "--disable-dns": true}
	flags, bools, rest, ok := f.dockerArgs(args, valued, boolean)
	if !ok || len(rest) != 1 {
		return dockerFail(stderr, "bad network create: %s", strings.Join(args, " "))
	}
	if bools["--disable-dns"] && f.driver != "podman" {
		f.t.Errorf("fake docker: --disable-dns is podman's")
	}
	name := rest[0]
	if f.nets[name] != nil {
		return dockerFail(stderr, "network with name %s already exists", name)
	}
	if len(flags["--subnet"]) == 0 {
		f.t.Errorf("fake %s: network create without --subnet: the driver places every network", f.driver)
		return dockerFail(stderr, "no subnet")
	}
	subnet, err := netip.ParsePrefix(flags["--subnet"][0])
	if err != nil {
		return dockerFail(stderr, "invalid subnet")
	}
	for _, n := range f.nets {
		if n.subnet.Overlaps(subnet) {
			return dockerFail(stderr, "Pool overlaps with other one on this address space")
		}
	}
	n := &fakeDNet{subnet: subnet, internal: bools["--internal"], labels: map[string]string{}, created: time.Now()}
	for _, l := range flags["--label"] {
		k, v, _ := strings.Cut(l, "=")
		n.labels[k] = v
	}
	f.nets[name] = n
	return nil
}

func (f *fakeDaemon) logs(ctx context.Context, stderr io.Writer, ref string) error {
	f.mu.Lock()
	c := f.lookup(ref)
	f.mu.Unlock()
	if c == nil {
		return dockerFail(stderr, "No such container: %s", ref)
	}
	if u := c.env["PATS_EGRESS_UPSTREAM_HTTPS"]; u != "" && f.upstreamDown {
		fmt.Fprintf(stderr, "error: upstream %s unreachable: connection refused\n", u)
		return nil
	}
	fmt.Fprintf(stderr, "listening %s\n", c.env["PATS_EGRESS_LISTEN"])
	select {
	case <-c.gone: // the stream ends with the container
		return nil
	case <-ctx.Done():
		return exitStatus(-1)
	}
}

var execValued = map[string]bool{"-u": true, "-w": true, "-e": true}

func (f *fakeDaemon) exec(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) error {
	flags, bools, rest, ok := f.dockerArgs(args, execValued, map[string]bool{"-i": true})
	if !ok || len(rest) < 2 || len(flags["-u"]) == 0 || len(flags["-w"]) == 0 {
		f.t.Errorf("fake %s: exec without -u, -w or a command: %v", f.driver, args)
		return dockerFail(stderr, "bad exec")
	}
	f.mu.Lock()
	c := f.lookup(rest[0])
	if c == nil {
		f.mu.Unlock()
		return dockerFail(stderr, "No such container: %s", rest[0])
	}
	xctx, kill := context.WithCancel(ctx)
	defer kill()
	env := kv(flags["-e"])
	// the env wrapper (envArgv): the env comes in on stdin, which -i keeps
	// open, as a shell would read it.
	if v := rest[1:]; len(v) >= 5 && v[0] == "/bin/sh" && v[1] == "-c" && v[3] == "pats-env" {
		if !bools["-i"] {
			f.t.Errorf("fake %s: env wrapper without -i: %v", f.driver, args)
		}
		script, _ := io.ReadAll(stdin)
		for k, val := range parseEnvScript(string(script)) {
			env[k] = val
		}
		rest = append(rest[:1:1], v[4:]...)
	}
	x := &fakeExec{inst: c.id, user: flags["-u"][0], env: env, kill: kill}
	f.execs[x] = true
	agent := f.agent
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.execs, x)
		f.mu.Unlock()
	}()
	argv := rest[1:]
	if len(argv) >= 4 && argv[0] == "/bin/sh" && argv[1] == "-c" && argv[3] == "handover" {
		return nil
	}
	if agent != nil {
		if err := agent(xctx, c.id, argv, stdout, stderr); err != nil {
			return err
		}
	}
	if xctx.Err() != nil {
		return exitStatus(137)
	}
	return nil
}

// resolve maps an in-container path to the host when a bind holds it.
func (c *fakeCtr) resolve(p string) (host string, ro, bound bool) {
	for _, b := range c.binds {
		if p == b.path || strings.HasPrefix(p, b.path+"/") {
			return filepath.Join(b.host, strings.TrimPrefix(p, b.path)), b.ro, true
		}
	}
	return "", false, false
}

// put writes a file at p in c, through a bind when one holds it.
func (c *fakeCtr) put(p string, b []byte) error {
	if host, ro, ok := c.resolve(p); ok {
		if ro {
			return fmt.Errorf("read-only file system")
		}
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			return err
		}
		return os.WriteFile(host, b, 0o644)
	}
	c.files[p] = b
	for d := path.Dir(p); d != "/"; d = path.Dir(d) {
		c.dirs[d] = true
	}
	return nil
}

func (c *fakeCtr) isDir(p string) bool {
	if host, _, ok := c.resolve(p); ok {
		st, err := os.Stat(host)
		return err == nil && st.IsDir()
	}
	return c.dirs[p]
}

// tree is every file at or under p in c, by path relative to p.
func (c *fakeCtr) tree(p string) map[string][]byte {
	out := map[string][]byte{}
	if host, _, ok := c.resolve(p); ok {
		_ = filepath.WalkDir(host, func(h string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				rel, _ := filepath.Rel(host, h)
				b, _ := os.ReadFile(h)
				out[filepath.ToSlash(rel)] = b
			}
			return nil
		})
		return out
	}
	for fp, b := range c.files {
		if fp == p {
			out["."] = b
		} else if strings.HasPrefix(fp, p+"/") {
			out[strings.TrimPrefix(fp, p+"/")] = b
		}
	}
	return out
}

// cp copies between the host and a container by docker cp's rules: a
// directory lands inside an existing directory under its own name, or is
// the destination when that does not exist, or has its contents copied with
// a /. source; a file lands inside an existing directory, or at the path.
func (f *fakeDaemon) cp(stderr io.Writer, src, dst string) error {
	if ref, p, ok := strings.Cut(dst, ":"); ok {
		c := f.lookup(ref)
		if c == nil {
			return dockerFail(stderr, "No such container: %s", ref)
		}
		contents := strings.HasSuffix(src, "/.")
		src = strings.TrimSuffix(src, "/.")
		st, err := os.Stat(src)
		if err != nil {
			return dockerFail(stderr, "%v", err)
		}
		if contents && !st.IsDir() {
			return dockerFail(stderr, "%s: not a directory", src)
		}
		if !st.IsDir() {
			if c.isDir(p) {
				p = path.Join(p, filepath.Base(src))
			}
			b, _ := os.ReadFile(src)
			return c.put(p, b)
		}
		if _, isFile := c.files[p]; isFile {
			return dockerFail(stderr, "cannot copy a directory over the file %s", p)
		}
		base := p
		if c.isDir(p) && !contents {
			base = path.Join(p, filepath.Base(src))
		}
		c.dirs[base] = true
		return filepath.WalkDir(src, func(h string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(src, h)
			b, _ := os.ReadFile(h)
			return c.put(path.Join(base, filepath.ToSlash(rel)), b)
		})
	}
	ref, p, ok := strings.Cut(src, ":")
	if !ok {
		f.t.Errorf("fake %s: cp with no container side: %s %s", f.driver, src, dst)
		return dockerFail(stderr, "bad cp")
	}
	c := f.lookup(ref)
	if c == nil {
		return dockerFail(stderr, "No such container: %s", ref)
	}
	contents := strings.HasSuffix(p, "/.")
	p = strings.TrimSuffix(p, "/.")
	files := c.tree(p)
	if len(files) == 0 && !c.isDir(p) {
		return dockerFail(stderr, "Could not find the file %s in container %s", p, ref)
	}
	if contents && !c.isDir(p) {
		return dockerFail(stderr, "%s: not a directory", p)
	}
	if st, err := os.Stat(dst); err == nil && !st.IsDir() && (contents || c.isDir(p)) {
		return dockerFail(stderr, "cannot copy a directory over the file %s", dst)
	}
	base := dst
	if st, err := os.Stat(dst); err == nil && st.IsDir() && !contents {
		base = filepath.Join(dst, path.Base(p))
	}
	for rel, b := range files {
		h := filepath.Join(base, filepath.FromSlash(rel))
		if rel == "." {
			h = base
		}
		if err := os.MkdirAll(filepath.Dir(h), 0o755); err != nil {
			return dockerFail(stderr, "%v", err)
		}
		if err := os.WriteFile(h, b, 0o644); err != nil {
			return dockerFail(stderr, "%v", err)
		}
	}
	return nil
}

// write puts a file in a container, as something running in it would.
func (f *fakeDaemon) write(ctr, p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.ctrs[ctr]; c != nil {
		if err := c.put(p, []byte(content)); err != nil {
			f.t.Errorf("fake %s: write %s: %v", f.driver, p, err)
		}
	}
}

// read reads a file out of a container.
func (f *fakeDaemon) read(ctr, p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.ctrs[ctr]
	if c == nil {
		return "", false
	}
	b, ok := c.tree(p)["."]
	if !ok {
		if host, _, bound := c.resolve(p); bound {
			if hb, err := os.ReadFile(host); err == nil {
				return string(hb), true
			}
		}
	}
	return string(b), ok
}

// leftovers is every container, every network but the default one, and
// every exec still running.
func (f *fakeDaemon) leftovers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.ctrs {
		out = append(out, "container "+c.image+" "+c.name)
	}
	for name := range f.nets {
		if name != "bridge" && name != "podman" {
			out = append(out, "network "+name)
		}
	}
	for x := range f.execs {
		out = append(out, "exec in "+x.inst[:12])
	}
	sort.Strings(out)
	return out
}

func hasLabel(labels []string, prefix string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) && len(l) > len(prefix) {
			return true
		}
	}
	return false
}
