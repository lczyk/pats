package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// the tests in this file run the lxd driver against fakeServer: what the
// server holds is the assertion, not which calls were made.

// egressMode is an egress setup a session can start under.
type egressMode struct {
	name   string
	egress Egress
}

// lxdEgressModes are the egress setups a session can start under.
var lxdEgressModes = []egressMode{
	{"none", Egress{None: true}},
	{"open", Egress{SelfTest: true}},
	{"proxy", Egress{Set: true, SelfTest: true, Policy: proxy.Policy{Allow: []string{"example.com"}}}},
	{"mitm", Egress{Set: true, SelfTest: true, Policy: proxy.Policy{Allow: []string{"example.com/x/*"}}}},
}

func proxyBin(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	return bin
}

// a whole session under each egress mode: start, a root step, the hand
// over, the agent, a sync, close; then the process ends and nothing of the
// run is left on the server.
func TestLXDSessionOnServer(t *testing.T) {
	for _, vm := range []bool{false, true} {
		for _, mode := range lxdEgressModes {
			t.Run(fmt.Sprintf("%s/vm=%v", mode.name, vm), func(t *testing.T) {
				f := newFakeServer(t)
				l := lxdOn(t, f, Options{VM: vm, ProxyBin: proxyBin(t)})
				work, ro := t.TempDir(), t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(work, "task.md"), []byte("do it"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(ro, "ref"), []byte("ref"), 0o644))
				var env map[string]string
				f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
					if argv[0] == "agent" {
						f.put(inst, WorkMount+"/out.txt", "done\n")
					}
					return nil
				}
				sess, err := l.Start(context.Background(), Spec{Workdir: work, Egress: mode.egress,
					Mounts: []Mount{{Host: ro, Container: "/ref", ReadOnly: true}}})
				require.NoError(t, err)
				s := sess.(*lxdSession)
				env = s.env

				got, ok := f.file(s.name, WorkMount+"/task.md")
				assert.That(t, ok && got == "do it", "workdir not pushed: %q", got)
				_, ok = f.file(s.name, "/ref/ref")
				assert.That(t, ok, "mount not pushed")
				switch mode.name {
				case "none":
					assert.Equal(t, s.bridge, "")
				case "open":
					assert.That(t, f.has("network", s.bridge), "no bridge")
				case "proxy", "mitm":
					assert.ContainsString(t, env["HTTP_PROXY"], ":"+proxyPort)
					assert.That(t, f.has("instance", s.name+"-p"), "no proxy")
				}
				if mode.name == "mitm" {
					_, ok = f.file(s.name, agentBundlePath)
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
				l.p.shutdown()
				assert.Equal(t, strings.Join(f.leftovers(l.p.token), "\n"), "")
			})
		}
	}
}

// an Exec cancelled mid-agent kills the agent's processes and keeps the
// instance; a cancelled root step stops it.
func TestLXDExecCancelOnServer(t *testing.T) {
	for _, root := range []bool{false, true} {
		f := newFakeServer(t)
		l := lxdOn(t, f, Options{})
		started := make(chan struct{})
		f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
			if argv[0] == "agent" {
				close(started)
				<-ctx.Done()
			}
			return nil
		}
		sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
		require.NoError(t, err)
		s := sess.(*lxdSession)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err = execBounded(t, sess, ctx, Cmd{Argv: []string{"agent"}, Root: root})
		require.NoError(t, err)
		f.mu.Lock()
		running := f.insts[s.name].running
		f.mu.Unlock()
		assert.Equal(t, running, !root, "root=%v", root)
		require.NoError(t, sess.Close())
	}
}

// a create the client was killed during finishes on the server after the
// client gave up; Close still removes it, for the agent and for the proxy.
func TestLXDKilledCreate(t *testing.T) {
	for _, which := range []string{"agent", "proxy"} {
		t.Run(which, func(t *testing.T) {
			f := newFakeServer(t)
			l := lxdOn(t, f, Options{ProxyBin: proxyBin(t)})
			work := t.TempDir()
			name := lxdName(work)
			if which == "proxy" {
				name += "-p"
			}
			f.slow[name] = 3
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := l.Start(ctx, Spec{Workdir: work, Egress: lxdEgressModes[2].egress})
			require.Error(t, err, "")
			l.p.shutdown()
			assert.Equal(t, strings.Join(f.leftovers(l.p.token), "\n"), "")
		})
	}
}

// two processes on one server: concurrent pairs get their own instance,
// bridge and subnet; two runs missing one image build it and both use it.
func TestLXDTwoProcesses(t *testing.T) {
	f := newFakeServer(t)
	// a driver per pair on a process per run, as internal/eval builds them.
	a, b := lxdOn(t, f, Options{ProxyBin: proxyBin(t)}), lxdOn(t, f, Options{ProxyBin: proxyBin(t)})
	type res struct {
		s   *lxdSession
		err error
	}
	ch := make(chan res, 4)
	for _, run := range []*lxd{a, a, b, b} {
		l := newLXD(run.p, run.r, run.o).on(run.s)
		go func() {
			sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: lxdEgressModes[2].egress})
			if err != nil {
				ch <- res{err: err}
				return
			}
			ch <- res{s: sess.(*lxdSession)}
		}()
	}
	names, bridges, ips := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var sessions []*lxdSession
	for range 4 {
		r := <-ch
		require.NoError(t, r.err)
		sessions = append(sessions, r.s)
		assert.That(t, !names[r.s.name] && !bridges[r.s.bridge], "shared instance or bridge")
		names[r.s.name], bridges[r.s.bridge] = true, true
		ips[r.s.egress.ip.String()] = true
	}
	// one subnet per pair within a process; the server keeps two
	// processes' bridges apart, so their addresses may repeat.
	assert.That(t, len(ips) >= 2, "pairs of one process share a subnet")
	f.mu.Lock()
	proxyImages := 0
	for _, im := range f.images {
		for _, al := range im.aliases {
			if strings.HasPrefix(al, "pats-img-") {
				proxyImages++
			}
		}
	}
	f.mu.Unlock()
	assert.Equal(t, proxyImages, 1)
	for _, s := range sessions {
		require.NoError(t, s.Close())
	}
	a.p.shutdown()
	b.p.shutdown()
	assert.Equal(t, strings.Join(f.leftovers(a.p.token), "\n"), "")
	assert.Equal(t, strings.Join(f.leftovers(b.p.token), "\n"), "")
}

// a clean by another process leaves a live run alone and takes a dead one.
func TestLXDCleanOnServer(t *testing.T) {
	f := newFakeServer(t)
	live, dead := lxdOn(t, f, Options{}), lxdOn(t, f, Options{})
	ls, err := live.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	_, err = dead.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	// the dead run's process is gone: no Close, no shutdown, its lease is
	// old to a cleaner an hour on.
	cleaner := lxdOn(t, f, Options{Remote: "srv"})
	later := fixedClock{now: time.Now().Add(time.Hour)}
	cleaner.p.clock = later
	f.mu.Lock()
	f.acls[live.marker()].config["user.pats.lease"] = fmt.Sprint(later.now.Unix())
	f.mu.Unlock()
	require.NoError(t, cleaner.clean(context.Background(), false, false, io.Discard))
	assert.Equal(t, strings.Join(f.leftovers(dead.p.token), "\n"), "")
	assert.That(t, f.has("instance", ls.(*lxdSession).name), "a live run's instance was removed")
	require.NoError(t, ls.Close())
}

// sweep runs entry once per lxc call it makes, with that call faulted each
// way, until a run ends before the fault is reached. after each run the
// run's process ends, and nothing of the run may be left on the server.
func sweep(t *testing.T, entry func(t *testing.T, f *fakeServer, ctx context.Context) *process) {
	for _, kind := range faultKinds {
		for k := 0; ; k++ {
			f := newFakeServer(t)
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
			left := f.leftovers(p.token)
			if len(left) > 0 && isRemoval(call) {
				// a removal that fails leaves what it removes: the next
				// clean, once the run is dead, is what takes it.
				cleanLater(t, f)
				left = f.leftovers(p.token)
			}
			if len(left) > 0 {
				t.Errorf("%s at fault %d (lxc %s) left:\n  %s", kind.name, k, call, strings.Join(left, "\n  "))
				return
			}
			if !hit {
				assert.That(t, k > 0, "%s: the entry made no calls; the sweep tested nothing", kind.name)
				break
			}
		}
	}
}

// cleanLater runs pats clean from another process an hour on, when every run
// not renewing its lease is dead.
func cleanLater(t *testing.T, f *fakeServer) {
	f.faults.off()
	c := lxdOn(t, f, Options{Remote: "srv"})
	c.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
	require.NoError(t, c.clean(context.Background(), false, false, io.Discard))
}

// every call of Start, under every egress mode, failed or cancelled: what
// Start made is gone once its own cleanup and the process's end have run.
// the proxy modes build the proxy image too, so its build is swept here.
func TestLXDSweepStart(t *testing.T) {
	bin := proxyBin(t)
	for _, vm := range []bool{false, true} {
		for _, mode := range lxdEgressModes {
			t.Run(fmt.Sprintf("%s/vm=%v", mode.name, vm), func(t *testing.T) {
				sweep(t, func(t *testing.T, f *fakeServer, ctx context.Context) *process {
					l := lxdOn(t, f, Options{VM: vm, ProxyBin: bin})
					sess, err := l.Start(ctx, Spec{Workdir: t.TempDir(), Egress: mode.egress})
					f.faults.off()
					if err == nil {
						sess.Close()
					}
					return l.p
				})
			})
		}
	}
}

// a proxied sandbox whose agent may sudo also runs the hostile probe and
// writes the sudoers rule: every call of that Start and its hand over.
func TestLXDSweepStartSudo(t *testing.T) {
	bin := proxyBin(t)
	sweep(t, func(t *testing.T, f *fakeServer, ctx context.Context) *process {
		l := lxdOn(t, f, Options{ProxyBin: bin})
		sess, err := l.Start(ctx, Spec{Workdir: t.TempDir(), Sudo: true, Egress: lxdEgressModes[2].egress})
		if err == nil {
			_ = sess.HandOver(ctx, WorkMount)
		}
		f.faults.off()
		if err == nil {
			sess.Close()
		}
		return l.p
	})
}

// clean deletes a dead run's instance once, without the wait Close does
// for a create the server is still running, since clean is not to wait:
// such an instance survives that clean and goes in the next one
// (docs/spec/lxd-testing.md, seam 5).
func TestLXDCleanCreateInFlight(t *testing.T) {
	f := newFakeServer(t)
	dead := lxdOn(t, f, Options{})
	dead.p.t.instanceGone = 50 * time.Millisecond
	work := t.TempDir()
	f.slow[lxdName(work)] = 1 << 20 // still running when clean comes
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !f.has("acl", dead.marker()) || f.find("init ") == nil {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := dead.Start(ctx, Spec{Workdir: work, Egress: Egress{None: true}})
	require.Error(t, err, "")
	// Start's Close gave up on the create (instanceGone), and the run ended.
	crash(dead.p)
	f.mu.Lock()
	for _, op := range f.ops {
		op.left = 3 // the create ends during the clean
	}
	f.mu.Unlock()
	cleanLater(t, f)
	assert.That(t, f.has("instance", lxdName(work)), "the create in flight finished before the clean read the server")
	cleanLater(t, f)
	assert.Equal(t, strings.Join(f.leftovers(dead.p.token), "\n"), "")
}

// every call of an image build: the builder and anything published under
// the run's private alias are gone; the cached image may stay.
func TestLXDSweepImage(t *testing.T) {
	sweep(t, func(t *testing.T, f *fakeServer, ctx context.Context) *process {
		l := lxdOn(t, f, Options{})
		_, _, _, _, _ = l.image(ctx, lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}, io.Discard)
		return l.p
	})
}

// every call of Close, each mode: what a failed call left, the next clean
// takes.
func TestLXDSweepClose(t *testing.T) {
	bin := proxyBin(t)
	for _, mode := range lxdEgressModes {
		t.Run(mode.name, func(t *testing.T) {
			sweep(t, func(t *testing.T, f *fakeServer, ctx context.Context) *process {
				plan := f.faults.plan
				f.faults.off()
				l := lxdOn(t, f, Options{ProxyBin: bin})
				sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: mode.egress})
				require.NoError(t, err)
				f.faults.arm(plan.at, plan.after, plan.cancel)
				sess.Close()
				f.faults.off()
				return l.p
			})
		})
	}
}

// crash ends p as a killed pats does: no Close, no shutdown, the lease no
// longer renewed.
func crash(p *process) {
	x := &p.lxd
	x.mu.Lock()
	defer x.mu.Unlock()
	for k, srv := range x.servers {
		if srv.stop != nil {
			close(srv.stop)
		}
		delete(x.servers, k)
	}
}

// every call of a clean of a dead run's leftovers: whatever a failed call
// left, the next clean takes.
func TestLXDSweepClean(t *testing.T) {
	bin := proxyBin(t)
	sweep(t, func(t *testing.T, f *fakeServer, ctx context.Context) *process {
		plan := f.faults.plan
		f.faults.off()
		dead := lxdOn(t, f, Options{ProxyBin: bin})
		_, err := dead.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: lxdEgressModes[2].egress})
		require.NoError(t, err)
		crash(dead.p)
		c := lxdOn(t, f, Options{Remote: "srv"})
		c.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
		f.faults.arm(plan.at, plan.after, plan.cancel)
		_ = c.clean(ctx, false, false, io.Discard)
		f.faults.off()
		cleanLater(t, f)
		return dead.p
	})
}

// another run makes what a run is about to make on first use (the pats
// project, the uplink) between its check and its create: the create fails,
// and the run goes on with the other's.
func TestLXDFirstUseRace(t *testing.T) {
	f := newFakeServer(t)
	f.before = func(args []string) {
		c := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(c, "project create "):
			f.projects[lxdProject] = true
		case strings.HasPrefix(c, "network create srv:"+lxdUplink+" "):
			f.nets[lxdUplink] = &fakeNet{config: map[string]string{"ipv4.address": "10.2.0.1/24"}}
		}
	}
	l := lxdOn(t, f, Options{ProxyBin: proxyBin(t)})
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: lxdEgressModes[2].egress})
	require.NoError(t, err)
	require.NoError(t, sess.Close())
}

// the lease, on clocks that move only when the test says (the requirements
// in docs/spec/fizz/lxd_lease.fizz): a live run renewing on time survives
// every clean, from a cleaner on its clock or up to 7.5 minutes ahead (R1);
// once its process dies, the first clean after the lease goes stale takes it
// (R2); and the accepted gap: a cleaner further ahead takes a live run.
func TestLXDLease(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	setup := func(t *testing.T) (*fakeServer, *lxd, *manualClock) {
		f := newFakeServer(t)
		mc := newManualClock(start)
		l := lxdOn(t, f, Options{})
		l.p.clock = mc
		l.p.t.leaseEvery, l.p.t.leaseStale = 2*time.Minute, 10*time.Minute
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
		require.NoError(t, err)
		return f, l, mc
	}
	lease := func(f *fakeServer, l *lxd) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		if a := f.acls[l.marker()]; a != nil {
			return a.config["user.pats.lease"]
		}
		return ""
	}
	// tick moves the run's clock on by 30s and waits for a renewal that
	// fell due to land, as one on time does.
	tick := func(t *testing.T, f *fakeServer, l *lxd, mc *manualClock) {
		mc.Advance(30 * time.Second)
		now := mc.Now()
		if now.Sub(start)%(2*time.Minute) != 0 {
			return
		}
		want := fmt.Sprint(now.Unix())
		deadline := time.Now().Add(5 * time.Second)
		for lease(f, l) != want {
			require.That(t, time.Now().Before(deadline), "the renewal due at %s did not land", now)
			time.Sleep(time.Millisecond)
		}
	}
	clean := func(t *testing.T, f *fakeServer, at time.Time) {
		c := lxdOn(t, f, Options{Remote: "srv"})
		c.p.clock = fixedClock{now: at}
		c.p.t.leaseStale = 10 * time.Minute
		require.NoError(t, c.clean(context.Background(), false, false, io.Discard))
	}
	alive := func(f *fakeServer, l *lxd) bool { return len(f.leftovers(l.p.token)) > 0 }

	t.Run("R1 a live run survives", func(t *testing.T) {
		for _, ahead := range []time.Duration{0, 7*time.Minute + 30*time.Second} {
			f, l, mc := setup(t)
			for range 60 {
				tick(t, f, l, mc)
				clean(t, f, mc.Now().Add(ahead))
				require.That(t, alive(f, l), "a cleaner %s ahead took a live run at %s", ahead, mc.Now().Sub(start))
			}
		}
	})
	t.Run("R2 a dead run goes", func(t *testing.T) {
		f, l, mc := setup(t)
		for range 5 {
			tick(t, f, l, mc)
		}
		crash(l.p)
		for i := 0; alive(f, l); i++ {
			require.That(t, i < 21, "a dead run outlived its stale lease")
			mc.Advance(30 * time.Second)
			clean(t, f, mc.Now())
		}
		// the last renewal landed at 2m; it is stale past 12m.
		assert.That(t, mc.Now().Sub(start) > 12*time.Minute, "a run taken before its lease went stale")
	})
	t.Run("a marker gone is made again at the next renewal", func(t *testing.T) {
		f, l, mc := setup(t)
		f.mu.Lock()
		delete(f.acls, l.marker())
		f.mu.Unlock()
		for range 4 {
			tick(t, f, l, mc)
		}
		f.mu.Lock()
		cfg := f.acls[l.marker()].config
		f.mu.Unlock()
		assert.Equal(t, cfg["user.pats.run"], l.p.token)
		clean(t, f, mc.Now())
		assert.That(t, alive(f, l), "a live run whose marker was made again was taken")
	})
	t.Run("accepted gap: a cleaner 9m ahead takes a live run", func(t *testing.T) {
		f, l, mc := setup(t)
		for range 3 { // the marker's lease, made at 0, is 1m30s old
			tick(t, f, l, mc)
		}
		clean(t, f, mc.Now().Add(9*time.Minute))
		assert.That(t, !alive(f, l), "the gap the lease model accepts closed: update docs/spec/fizz/lxd_lease.fizz")
	})
}

// an image of another remote is copied onto the server once, into the pats
// project, and found there by its fingerprint after; force copies again.
func TestLXDCopyImageOnServer(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	fp, copied, err := l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	assert.Equal(t, fp, fakeFP("ubuntu:26.04"))
	assert.That(t, copied, "the first copy did not copy")
	got, err := l.fingerprint(context.Background(), fp)
	require.NoError(t, err)
	assert.Equal(t, got, fp)

	_, copied, err = l.copyImage(context.Background(), "ubuntu:26.04", false)
	require.NoError(t, err)
	assert.That(t, !copied, "a present image was copied again")
	assert.Equal(t, f.copies, 1)
	_, copied, err = l.copyImage(context.Background(), "ubuntu:26.04", true)
	require.NoError(t, err) // lxd's "already exists" is not an error here
	assert.That(t, copied, "force did not copy")

	// an unqualified ref is an image on the sandbox's server, not the cli default's.
	f.reset()
	got, err = l.fingerprint(context.Background(), "img")
	require.NoError(t, err)
	assert.Equal(t, got, fakeFP("img"))
	assert.ContainsString(t, strings.Join(f.call("query"), " "), "query srv:/1.0/images/aliases/img?project=pats")
}

// a base named without a remote is an image of the sandbox's own server: the
// builder launches from it there, by the fingerprint its key was read as.
func TestLXDImageOwnBaseOnServer(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	_, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "img", Argv: []string{"true"}}, io.Discard)
	require.NoError(t, err)
	// by the fingerprint its key was made of, never the name again.
	assert.ContainsString(t, strings.Join(f.call("launch"), " "), "launch srv:"+fakeFP("img")+" ")
}

// a build is published with what it is, for lxc image list; a forced build
// makes a new image and points the alias at it in one step, the old image
// staying for whoever pins it.
func TestLXDImagePropsAndForceOnServer(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{})
	spec := lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}, Role: "project", Sandbox: "mason"}
	alias, _, _, built, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.That(t, built, "the first build did not build")
	first := f.lookupImage(alias)
	props := f.images[first].props
	assert.Equal(t, props["pats.role"], "project")
	assert.Equal(t, props["pats.sandbox"], "mason")
	assert.Equal(t, props["pats.version"], version.Info.Version)
	assert.Equal(t, "pats-img-"+props["pats.inputs"], alias)

	_, _, _, built, err = l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.That(t, !built, "an unforced build of a present key built")

	spec.Force = true
	again, _, _, built, err := l.image(context.Background(), spec, io.Discard)
	require.NoError(t, err)
	assert.That(t, built && again == alias, "a forced build: built %v, alias %s", built, again)
	second := f.lookupImage(alias)
	assert.That(t, second != first, "the alias still names the old image")
	assert.That(t, f.images[first] != nil, "the old image went with the force")
	for _, im := range f.images {
		for _, a := range im.aliases {
			assert.That(t, !runPrivateAlias(a), "a run-private alias was left: %s", a)
		}
	}
}

// the key carries the pats version, and a Key stands for the material.
func TestImageKeyVersionAndKey(t *testing.T) {
	old := version.Info
	t.Cleanup(func() { version.Info = old })
	spec := lxdImageSpec{Argv: []string{"true"}}
	a, err := imageKey("fp", spec, false)
	require.NoError(t, err)
	version.Info.Version = "9.9.9"
	b, err := imageKey("fp", spec, false)
	require.NoError(t, err)
	assert.That(t, a != b, "a new pats version kept the key")

	x, _ := imageKey("fp", lxdImageSpec{Key: "proxy 9.9.9 amd64", Argv: []string{"a"}}, false)
	y, _ := imageKey("fp", lxdImageSpec{Key: "proxy 9.9.9 amd64", Argv: []string{"b"}}, false)
	assert.Equal(t, x, y)
}
