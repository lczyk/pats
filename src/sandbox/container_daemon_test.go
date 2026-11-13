package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

var containerModes = lxdEgressModes

// a whole session under each egress mode, docker and podman: the agent's
// writes reach the host through the bound workdir, and nothing is left
// after Close.
func TestContainerSessionOnDaemon(t *testing.T) {
	for _, driver := range Engines() {
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

// containerSweep is sweep for the container driver: one entry run per call
// it reaches, each fault kind. nothing may be left once the run's process
// ends, or, where the faulted call was itself a removal, once a later
// clean has run.
func containerSweep(t *testing.T, driver string, entry func(t *testing.T, f *fakeDaemon, ctx context.Context) *process) {
	for _, kind := range faultKinds {
		for k := 0; ; k++ {
			f := newFakeDaemon(t, driver)
			ctx, cancel := context.WithCancel(context.Background())
			var stop context.CancelFunc
			if kind.cancel {
				stop = cancel
			}
			f.faults.arm(k, kind.after, stop)
			p := entry(t, f, ctx)
			cancel()
			p.shutdown()
			hit, call := f.faults.reached()
			left := f.leftovers()
			if len(left) > 0 && isRemoval(call) {
				containerCleanLater(t, f, p)
				left = f.leftovers()
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

// containerCleanLater runs pats clean on f from another process of run's
// user an hour on, when every run not renewing its lease is dead.
func containerCleanLater(t *testing.T, f *fakeDaemon, run *process) {
	f.faults.off()
	c := newContainer(testProcess(t), runFunc(f.run), Options{Driver: f.driver})
	c.p.home = run.home
	c.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
	_, err := c.p.clean(context.Background(), c, CleanOptions{Place: f.driver})
	require.NoError(t, err)
}

// containerRun is a driver and egress mode a container sweep covers.
type containerRun struct {
	driver string
	mode   egressMode
}

func containerRuns() []containerRun {
	var out []containerRun
	for _, d := range Engines() {
		for _, m := range containerModes {
			out = append(out, containerRun{d, m})
		}
	}
	return out
}

// every call of Start under every egress mode: what a failed or cancelled
// Start made is gone once it returns.
func TestContainerSweepStart(t *testing.T) {
	for _, run := range containerRuns() {
		mode, driver := run.mode, run.driver
		t.Run(driver+"/"+mode.name, func(t *testing.T) {
			containerSweep(t, driver, func(t *testing.T, f *fakeDaemon, ctx context.Context) *process {
				c := containerOn(t, f)
				sess, err := c.Start(ctx, Spec{Workdir: workdir(t), Egress: mode.egress})
				f.faults.off()
				if err == nil {
					sess.Close()
				}
				return c.p
			})
		})
	}
}

// every call of Close: what a failed call left, pats clean finds.
func TestContainerSweepClose(t *testing.T) {
	for _, run := range containerRuns() {
		mode, driver := run.mode, run.driver
		t.Run(driver+"/"+mode.name, func(t *testing.T) {
			containerSweep(t, driver, func(t *testing.T, f *fakeDaemon, ctx context.Context) *process {
				plan := f.faults.plan
				f.faults.off()
				c := containerOn(t, f)
				sess, err := c.Start(context.Background(), Spec{Workdir: workdir(t), Egress: mode.egress})
				require.NoError(t, err)
				f.faults.arm(plan.at, plan.after, plan.cancel)
				sess.Close()
				f.faults.off()
				return c.p
			})
		})
	}
}

// every call of a clean of a dead run's leftovers: whatever a failed call
// left, the next clean takes.
func TestContainerSweepClean(t *testing.T) {
	for _, driver := range Engines() {
		t.Run(driver, func(t *testing.T) {
			containerSweep(t, driver, func(t *testing.T, f *fakeDaemon, ctx context.Context) *process {
				plan := f.faults.plan
				f.faults.off()
				dead := containerOn(t, f)
				_, err := dead.Start(context.Background(), Spec{Workdir: workdir(t), Egress: containerModes[len(containerModes)-1].egress})
				require.NoError(t, err)
				crashLease(dead.p)
				c := newContainer(testProcess(t), runFunc(f.run), Options{Driver: driver})
				c.p.home = dead.p.home
				c.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
				f.faults.arm(plan.at, plan.after, plan.cancel)
				_, _ = c.p.clean(ctx, c, CleanOptions{Place: driver})
				f.faults.off()
				containerCleanLater(t, f, dead.p)
				return dead.p
			})
		})
	}
}

// crashLease ends p's lease renewal as a crash does: the file stays, and
// goes stale. it then names another host, since this test's own pid is
// alive and a live pid here keeps a run alive.
func crashLease(p *process) {
	if p.lease.stop != nil {
		close(p.lease.stop)
		<-p.lease.done
		p.lease.stop = nil
	}
	path, err := p.leasePath(p.token)
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("elsewhere 1\n"), 0o644)
	_ = os.Chtimes(path, info.ModTime(), info.ModTime())
}
