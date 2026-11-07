package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// probe is a script a driver runs in a sandbox and reads the output of. it
// runs as `<shell> -c <script> <name> <args>...`: the name is the script's $0,
// so a fake answers by it and a shell error names the probe, and the args are
// $1 onwards, so the script itself is constant.
type probe struct {
	name   string
	shell  string // sh, or bash where the script needs /dev/tcp
	script string
}

func (p probe) argv(args ...string) []string {
	return append([]string{p.shell, "-c", p.script, p.name}, args...)
}

var (
	// probeReady: exec works (a vm's agent is up after boot).
	probeReady = probe{"ready", "sh", "true"}
	// probeRoute: exec works and the instance has a default route.
	probeRoute = probe{"route", "sh", "ip -4 route show default | grep -q ."}
	// probeOnline: a tcp connection out to the internet. an instance with no
	// bash cannot be probed and passes.
	probeOnline = probe{"online", "sh", `command -v bash >/dev/null 2>&1 || exit 0; timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null || timeout 5 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>/dev/null`}
	// probeCanary: every way out with the proxy env unset; see canaryVerdict.
	probeCanary = probe{"canary", "bash", canaryScript}
	// probeReach: the proxy at $1 port $2 answers; see reachVerdict. the
	// inner bash gets them as its own args, so they stay data.
	probeReach = probe{"reach", "bash", `timeout 5 bash -c 'exec 3<>/dev/tcp/$1/$2' reach "$1" "$2" 2>/dev/null && echo "egress-reach ok=1" || echo "egress-reach ok=0"`}
	// probeHostile: as root, a default route through the bridge's host side
	// ($1, at mac $2) and a try out; see hostileVerdict.
	probeHostile = probe{"hostile", "sh", `ip neigh replace "$1" lladdr "$2" dev pats0 nud permanent && ip route replace default via "$1" dev pats0 && { timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && echo "egress-hostile leak=1" || echo "egress-hostile leak=0"; }; ip route del default 2>/dev/null; ip neigh del "$1" dev pats0 2>/dev/null; true`}
	// probeAgentUser: the agent user, made when the image lacks it; prints
	// its uid last.
	probeAgentUser = probe{"agent-user", "sh", `id -u ` + lxdAgentUser + ` >/dev/null 2>&1 || useradd -m -s /bin/bash ` + lxdAgentUser + `; id -u ` + lxdAgentUser}
	// probeProxyUser: the proxy user's uid.
	probeProxyUser = probe{"proxy-user", "sh", `id -u ` + lxdProxyUser}
)

// canaryScript probes every way out of the sandbox with the proxy env unset:
// tcp4, tcp6, a dns query over udp, and a name lookup. a leak prints leak=1.
// bash's /dev/tcp and /dev/udp need no tool in the image beyond bash itself;
// for udp only an answer counts, since connect and write cannot fail on it.
const canaryScript = `for c in timeout getent; do command -v $c >/dev/null 2>&1 || { echo "egress-canary missing=$c"; exit 0; }; done
leak=0
timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && leak=1
timeout 3 bash -c 'exec 3<>/dev/tcp/2606:4700:4700::1111/443' 2>/dev/null && leak=1
timeout 3 bash -c 'exec 3<>/dev/udp/8.8.8.8/53; printf "\000\001\001\000\000\001\000\000\000\000\000\000\007example\003com\000\000\001\000\001" >&3; read -t 2 -N 1 <&3' 2>/dev/null && leak=1
getent hosts example.com >/dev/null 2>&1 && leak=1
echo "egress-canary leak=$leak"`

// canaryVerdict reads the canary's output: nil when every probe failed.
func canaryVerdict(out, driver, image string) error {
	switch {
	case strings.Contains(out, "egress-canary leak=0"):
		return nil
	case strings.Contains(out, "egress-canary missing="):
		return fmt.Errorf("egress self-test could not run in image %s: %s", image, strings.TrimPrefix(out[strings.Index(out, "egress-canary missing="):], "egress-canary "))
	case strings.Contains(out, "egress-canary leak=1"):
		return fmt.Errorf("egress is not fail-closed under driver %s: a direct connection from the sandbox succeeded", driver)
	}
	return fmt.Errorf("egress self-test gave no verdict: %s", out)
}

// reachVerdict reads the reach probe's output: whether the proxy answered,
// and whether the probe said either way.
func reachVerdict(out string) (reached, said bool) {
	switch {
	case strings.Contains(out, "egress-reach ok=1"):
		return true, true
	case strings.Contains(out, "egress-reach ok=0"):
		return false, true
	}
	return false, false
}

// hostileVerdict reads the hostile probe's output: nil only when it said the
// route out failed.
func hostileVerdict(out string) error {
	if strings.Contains(out, "egress-hostile leak=0") {
		return nil
	}
	return fmt.Errorf("egress is not fail-closed under lxd: a root agent routing through the bridge's host side got out (%s)", strings.TrimSpace(out))
}

// uidVerdict reads a uid off the last line of out.
func uidVerdict(out string) (string, error) {
	uid := lastLine(strings.TrimSpace(out))
	if _, err := strconv.Atoi(uid); err != nil {
		return "", fmt.Errorf("uid %q", uid)
	}
	return uid, nil
}
