package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the tests in this file run the docker and podman driver against
// fakeDaemon: what the daemon holds is the assertion.

// workdir is a pair's workdir as a run makes it, under the pats temp root:
// the agent container is then named and labelled for pats clean.
func workdir(t *testing.T) string {
	dir, err := MkTemp("pats-work-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = RemoveTemp(dir) })
	return dir
}

var containerModes = []struct {
	name   string
	egress Egress
}{lxdEgressModes[0], lxdEgressModes[1], lxdEgressModes[2], lxdEgressModes[3]}

// a whole session under each egress mode, docker and podman: the agent's
// writes reach the host through the bound workdir, and nothing is left
// after Close.
func TestContainerSessionOnDaemon(t *testing.T) {
	for _, driver := range []string{"docker", "podman"} {
		for _, mode := range containerModes {
			t.Run(driver+"/"+mode.name, func(t *testing.T) {
				f := newFakeDaemon(t, driver)
				c := containerOn(t, f)
				work := workdir(t)
				var env map[string]string
				f.agent = func(ctx context.Context, ctr string, argv []string, stdout, stderr io.Writer) error {
					if argv[0] == "agent" {
						f.write(ctr, WorkMount+"/out.txt", "done\n")
					}
					return nil
				}
				sess, err := c.Start(context.Background(), Spec{Workdir: work, Egress: mode.egress})
				require.NoError(t, err)
				s := sess.(*containerSession)
				f.mu.Lock()
				ctr := f.ctrs[s.id]
				env = ctr.env
				_, onNone := ctr.nets["none"]
				f.mu.Unlock()
				switch mode.name {
				case "none":
					assert.That(t, len(ctr.nets) == 0 && !onNone, "a none sandbox on a network: %v", ctr.nets)
				case "proxy", "mitm":
					assert.ContainsString(t, env["HTTP_PROXY"], ":"+proxyPort)
				}
				if mode.name == "mitm" {
					_, ok := f.read(s.id, agentBundlePath)
					assert.That(t, ok, "no trust bundle in the agent")
				}
				code, err := sess.Exec(context.Background(), Cmd{Argv: []string{"setup"}, Root: true}, io.Discard, io.Discard)
				require.NoError(t, err)
				assert.Equal(t, code, 0)
				require.NoError(t, sess.HandOver(context.Background(), WorkMount))
				code, err = sess.Exec(context.Background(), Cmd{Argv: []string{"agent"}}, io.Discard, io.Discard)
				require.NoError(t, err)
				assert.Equal(t, code, 0)
				require.NoError(t, sess.Sync(context.Background()))
				b, err := os.ReadFile(filepath.Join(work, "out.txt"))
				require.NoError(t, err)
				assert.Equal(t, string(b), "done\n")
				require.NoError(t, sess.Close())
				assert.Equal(t, strings.Join(f.leftovers(), "\n"), "")
			})
		}
	}
}

// findable says whether pats clean finds a leftover: an sbx- name and the
// owner label.
func (f *fakeDaemon) findable() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lost []string
	for _, c := range f.ctrs {
		if !strings.HasPrefix(c.name, namePrefix) || c.labels["pats.owner"] == "" {
			lost = append(lost, "container "+c.image+" "+c.name)
		}
	}
	for name, n := range f.nets {
		if name != "bridge" && name != "podman" && (!strings.HasPrefix(name, namePrefix) || n.labels["pats.owner"] == "") {
			lost = append(lost, "network "+name)
		}
	}
	return lost
}

// containerSweep is sweep for the container driver: one entry run per call
// it reaches, each fault kind. nothing may be left, or, where the faulted
// call was itself a removal, only what pats clean finds.
func containerSweep(t *testing.T, entry func(t *testing.T, f *fakeDaemon, ctx context.Context)) {
	for _, kind := range faultKinds {
		for k := 0; ; k++ {
			f := newFakeDaemon(t, "docker")
			ctx, cancel := context.WithCancel(context.Background())
			var stop context.CancelFunc
			if kind.cancel {
				stop = cancel
			}
			f.faults.arm(k, kind.after, stop)
			entry(t, f, ctx)
			cancel()
			hit, call := f.faults.reached()
			left := f.leftovers()
			if len(left) > 0 && isRemoval(call) {
				left = f.findable()
			}
			if len(left) > 0 {
				t.Errorf("%s at fault %d (%s) left:\n  %s", kind.name, k, call, strings.Join(left, "\n  "))
				return
			}
			if !hit {
				assert.That(t, k > 0, "%s: the entry made no calls; the sweep tested nothing", kind.name)
				break
			}
		}
	}
}

// every call of Start under every egress mode: what a failed or cancelled
// Start made is gone once it returns.
func TestContainerSweepStart(t *testing.T) {
	for _, mode := range containerModes {
		t.Run(mode.name, func(t *testing.T) {
			containerSweep(t, func(t *testing.T, f *fakeDaemon, ctx context.Context) {
				sess, err := containerOn(t, f).Start(ctx, Spec{Workdir: workdir(t), Egress: mode.egress})
				f.faults.off()
				if err == nil {
					sess.Close()
				}
			})
		})
	}
}

// every call of Close: what a failed call left, pats clean finds.
func TestContainerSweepClose(t *testing.T) {
	for _, mode := range containerModes {
		t.Run(mode.name, func(t *testing.T) {
			containerSweep(t, func(t *testing.T, f *fakeDaemon, ctx context.Context) {
				plan := f.faults.plan
				f.faults.off()
				sess, err := containerOn(t, f).Start(context.Background(), Spec{Workdir: workdir(t), Egress: mode.egress})
				require.NoError(t, err)
				f.faults.arm(plan.at, plan.after, plan.cancel)
				sess.Close()
				f.faults.off()
			})
		})
	}
}
