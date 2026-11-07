package sandbox

import (
	"net"
	"os/exec"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a probe runs as its shell with the script, its name as $0 and its args
// after, so a fake can answer by name.
func TestProbeArgv(t *testing.T) {
	assert.Equal(t, strings.Join(probeReach.argv("10.0.0.2", "8080"), "|"),
		"bash|-c|"+probeReach.script+"|reach|10.0.0.2|8080")
	names := map[string]bool{}
	for _, p := range []probe{probeReady, probeRoute, probeOnline, probeCanary, probeReach, probeHostile, probeAgentUser, probeProxyUser} {
		assert.That(t, !names[p.name], "two probes named %s", p.name)
		names[p.name] = true
		assert.That(t, p.shell == "sh" || p.shell == "bash", "probe %s shell %s", p.name, p.shell)
	}
}

func TestCanaryVerdict(t *testing.T) {
	for _, tc := range []struct{ out, err string }{
		{"egress-canary leak=0\n", ""},
		{"egress-canary leak=1\n", "not fail-closed under driver lxd"},
		{"egress-canary missing=getent\n", "could not run in image img: missing=getent"},
		{"bash: not found", "no verdict"},
		{"", "no verdict"},
	} {
		err := canaryVerdict(tc.out, "lxd", "img")
		if tc.err == "" {
			assert.NoError(t, err)
		} else {
			assert.Error(t, err, tc.err)
		}
	}
}

func TestReachVerdict(t *testing.T) {
	for _, tc := range []struct {
		out           string
		reached, said bool
	}{
		{"egress-reach ok=1\n", true, true},
		{"egress-reach ok=0\n", false, true},
		{"bash: command not found", false, false},
		{"", false, false},
	} {
		reached, said := reachVerdict(tc.out)
		assert.Equal(t, reached, tc.reached, "%q", tc.out)
		assert.Equal(t, said, tc.said, "%q", tc.out)
	}
}

func TestHostileVerdict(t *testing.T) {
	assert.NoError(t, hostileVerdict("egress-hostile leak=0\n"))
	assert.Error(t, hostileVerdict("egress-hostile leak=1\n"), "not fail-closed")
	assert.Error(t, hostileVerdict("RTNETLINK answers: Operation not permitted\n"), "Operation not permitted")
}

func TestUIDVerdict(t *testing.T) {
	uid, err := uidVerdict("useradd: warning\n1001\n")
	require.NoError(t, err)
	assert.Equal(t, uid, "1001")
	_, err = uidVerdict("id: 'pats': no such user\n")
	assert.Error(t, err, "uid")
	_, err = uidVerdict("")
	assert.Error(t, err, "uid")
}

// the reach probe, run by a real bash against a port that answers and one
// that does not, says which.
func TestProbeReachRuns(t *testing.T) {
	for _, tool := range []string{"bash", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	argv := probeReach.argv(host, port)
	out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	reached, _ := reachVerdict(string(out))
	assert.That(t, reached, "open port not reached: %s", out)

	ln.Close()
	out, _ = exec.Command(argv[0], argv[1:]...).CombinedOutput()
	reached, said := reachVerdict(string(out))
	assert.That(t, !reached && said, "closed port: %s", out)
}
