package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

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
