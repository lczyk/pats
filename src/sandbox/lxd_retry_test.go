package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

func TestTransientText(t *testing.T) {
	for s, want := range map[string]bool{
		"Error: read tcp 10.0.0.2:51234->10.0.0.1:8443: read: connection reset by peer": true,
		"write: broken pipe": true,
		"dial tcp 10.0.0.1:8443: connect: connection refused": true,
		"dial tcp: i/o timeout":                               true,
		"connect: no route to host":                           true,
		"net/http: TLS handshake timeout":                     true,
		"Error: unexpected EOF":                               true,
		"websocket: close 1006 (abnormal closure)":            true,
		"Error: Instance not found":                           false,
		"Error: Network already exists":                       false,
		"Error: Failed to create instance: in use":            false,
		"Error: Certificate is restricted":                    false,
	} {
		assert.Equal(t, transientText(s), want, s)
	}
}

func TestTransientTarLine(t *testing.T) {
	// a name the instance chose cannot read as lxc's line.
	assert.That(t, !transientTarLine("tar: ./connection reset by peer: file changed as we read it\n"), "tar diagnostic")
	assert.That(t, transientTarLine("tar: ./a: something\nError: read tcp ...:8443: read: connection reset by peer\n"), "lxc line")
	assert.That(t, !transientTarLine("Error: read: connection reset by peer\ntar: done\n"), "not the last line")
}

// the lxc wrapper marks a transient failure of one call; an exec's output
// is never evidence.
func TestLXCPlace(t *testing.T) {
	l, _ := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		io.WriteString(stderr, "Error: read: connection reset by peer")
		return &fakeExit{}
	})
	_, err := l.lxc(context.Background(), "query", "srv:/1.0")
	assert.That(t, errors.Is(err, ErrPlace), "query: %v", err)
	assert.Equal(t, placeTries(err), 1)
	_, err = l.lxc(context.Background(), "exec", "srv:x", "--", "true")
	assert.That(t, !errors.Is(err, ErrPlace), "exec: %v", err)
}

func TestLXDRetry(t *testing.T) {
	l, _ := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	transient := func() error {
		return asPlace(errors.New("lxc query: connection reset by peer"), true)
	}
	t.Run("three tries", func(t *testing.T) {
		n := 0
		err := l.retry(context.Background(), func() error { n++; return transient() })
		assert.Equal(t, n, 3)
		assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
		assert.Equal(t, placeTries(err), 3)
		assert.ContainsString(t, err.Error(), "(3 tries, transient)")
	})
	t.Run("a non-transient failure at once", func(t *testing.T) {
		n := 0
		err := l.retry(context.Background(), func() error { n++; return errors.New("Error: not found") })
		assert.Equal(t, n, 1)
		assert.That(t, !errors.Is(err, ErrPlace), "ErrPlace: %v", err)
	})
	t.Run("a success after one", func(t *testing.T) {
		n := 0
		err := l.retry(context.Background(), func() error {
			if n++; n == 1 {
				return transient()
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, n, 2)
	})
	t.Run("ctx ends mid-backoff", func(t *testing.T) {
		l, _ := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
		l.p.t.lxdBackoff = []time.Duration{time.Hour}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		n := 0
		err := l.retry(ctx, func() error { n++; return transient() })
		assert.Equal(t, n, 1)
		assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
		assert.That(t, errors.Is(err, context.DeadlineExceeded), "no deadline: %v", err)
	})
	t.Run("used-up tries are not tried again", func(t *testing.T) {
		n := 0
		err := l.retry(context.Background(), func() error {
			n++
			return l.retry(context.Background(), transient)
		})
		assert.Equal(t, n, 1)
		assert.Equal(t, placeTries(err), 3)
	})
}

// a pull whose stream was reset is tried again into a fresh dir; a refused
// name with a clean exec is not.
func TestLXDPullRetried(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"})
	host := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(host, "old"), []byte("x"), 0o644))
	sess, err := l.Start(context.Background(), Spec{Workdir: host})
	require.NoError(t, err)
	defer sess.Close()
	pulls := 0
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "tar -c") {
			if pulls++; pulls == 1 {
				f.record(args)
				stdout.Write([]byte("partial"))
				io.WriteString(stderr, "Error: read tcp 1.2.3.4:8443: read: connection reset by peer\n")
				return exitStatus(1)
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	require.NoError(t, sess.Sync(context.Background()))
	assert.Equal(t, pulls, 2)
	b, err := os.ReadFile(filepath.Join(host, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(b), "hi\n")
	ents, _ := os.ReadDir(filepath.Dir(host))
	for _, e := range ents {
		assert.That(t, !strings.Contains(e.Name(), ".sync-"), "a try's dir was left: %s", e.Name())
	}

	pulls = 0
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "tar -c") {
			pulls++
			stdout.Write([]byte("not a tar at all, and long enough to be read as a header block............"))
			return nil
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	err = sess.Sync(context.Background())
	require.Error(t, err, "pull")
	assert.Equal(t, pulls, 1)
	assert.That(t, !errors.Is(err, ErrPlace), "a bad stream is ErrPlace: %v", err)
}

// a delete that gives up names what it last saw.
func TestLXDDeleteInstanceGivesUp(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "query srv:/1.0/operations"):
			io.WriteString(stderr, "Error: read: connection reset by peer")
			return &fakeExit{}
		case strings.HasPrefix(joined, "delete srv:pats-x"):
			io.WriteString(stderr, "Error: write: broken pipe")
			return &fakeExit{}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := l.deleteInstance(ctx, "pats-x")
	require.Error(t, err, "outlived")
	assert.ContainsString(t, err.Error(), "last delete: lxc delete srv:pats-x")
	assert.ContainsString(t, err.Error(), "broken pipe")
	assert.ContainsString(t, err.Error(), "last operations query: lxc query")
	assert.ContainsString(t, err.Error(), "connection reset by peer")
}

// a pair whose cleanup gave up is tried once more at shutdown: the
// instance, then its bridge and acl.
func TestLXDDeferredDeletes(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"})
	l.p.t.instanceGone = 30 * time.Millisecond
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
	require.NoError(t, err)
	s := sess.(*lxdSession)
	down := true
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ")
		if down && (strings.HasPrefix(joined, "query srv:/1.0/operations") || strings.HasPrefix(joined, "delete ") ||
			strings.HasPrefix(joined, "network delete") || strings.HasPrefix(joined, "network acl delete srv:patsbr")) {
			f.record(args)
			io.WriteString(stderr, "Error: read: connection reset by peer")
			return &fakeExit{}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	require.Error(t, sess.Close(), "outlived")
	down = false
	f.reset()
	l.p.shutdown()
	var order []string
	for _, c := range f.all() {
		switch {
		case strings.HasPrefix(c, "delete srv:"+s.name):
			order = append(order, "instance")
		case strings.HasPrefix(c, "network delete srv:"+s.bridge):
			order = append(order, "bridge")
		case strings.HasPrefix(c, "network acl delete srv:"+s.bridge):
			order = append(order, "acl")
		}
	}
	assert.Equal(t, strings.Join(order, " "), "instance bridge acl")
}

// resetOn makes the nth call whose joined args start with prefix fail on a
// lost link (n 0: every one), counting them.
func resetOn(l *lxd, f *fakeLXC, prefix string, n int) *int {
	seen := 0
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if strings.HasPrefix(strings.Join(args, " "), prefix) {
			if seen++; n == 0 || seen == n {
				f.record(args)
				io.WriteString(stderr, "Error: read tcp 1.2.3.4:8443: read: connection reset by peer")
				return &fakeExit{}
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	return &seen
}

func count(f *fakeLXC, prefix string) int {
	n := 0
	for _, c := range f.all() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// a transient init is tried again from a removed instance, on new names, and
// says so.
func TestLXDStartRetried(t *testing.T) {
	for _, step := range []string{"init ", "config device add", "start "} {
		t.Run(step, func(t *testing.T) {
			l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
			require.NoError(t, l.prepareServer(context.Background()))
			resetOn(l, f, step, 1)
			var lines []string
			sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Progress: func(m string) { lines = append(lines, m) }})
			require.NoError(t, err)
			s := sess.(*lxdSession)
			assert.Equal(t, count(f, "init "), 2)
			assert.That(t, count(f, "delete srv:"+s.name) >= 1, "the failed try was not removed")
			assert.Equal(t, s.bridge, l.bridgeName(s.name, 2))
			assert.Equal(t, len(lines), 1)
			assert.ContainsString(t, lines[0], "trying again in")
			assert.ContainsString(t, lines[0], "(try 2/3)")
			require.NoError(t, sess.Close())
			// the first try's bridge, once made, waits for shutdown too.
			f.reset()
			l.p.shutdown()
			if step != "init " {
				assert.That(t, count(f, "network delete srv:"+l.bridgeName(s.name, 1)) == 1, "the first try's bridge was not deferred")
			}
		})
	}
}

// a read that already had its tries is not tried again by the start; one
// that keeps failing ends it after three.
func TestLXDStartNotRetried(t *testing.T) {
	t.Run("a used-up read", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		require.NoError(t, l.prepareServer(context.Background()))
		resetOn(l, f, "query srv:/1.0/instances/", 0)
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
		require.Error(t, err, "(3 tries, transient)")
		assert.That(t, errors.Is(err, ErrPlace) && errors.Is(err, ErrLaunch), "not ErrPlace: %v", err)
		assert.Equal(t, count(f, "init "), 1)
	})
	t.Run("an init down three times", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		require.NoError(t, l.prepareServer(context.Background()))
		resetOn(l, f, "init ", 0)
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
		require.Error(t, err, "connection reset")
		assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
		assert.Equal(t, count(f, "init "), 3)
		assert.ContainsString(t, err.Error(), "(3 tries, transient)")
	})
	t.Run("a removal that gives up", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		l.p.t.launchCleanup = 30 * time.Millisecond
		require.NoError(t, l.prepareServer(context.Background()))
		inner := f.run
		l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			j := strings.Join(args, " ")
			if strings.HasPrefix(j, "init ") || strings.HasPrefix(j, "query srv:/1.0/operations") {
				f.record(args)
				io.WriteString(stderr, "Error: read: connection reset by peer")
				return &fakeExit{}
			}
			return inner(ctx, stdin, stdout, stderr, args...)
		})
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
		require.Error(t, err, "could not be removed")
		assert.Equal(t, count(f, "init "), 1)
	})
	t.Run("a full disk under push", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		require.NoError(t, l.prepareServer(context.Background()))
		f.cutPushes = 100
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
		require.Error(t, err, "arrived short")
		assert.That(t, !errors.Is(err, ErrPlace), "ErrPlace: %v", err)
		assert.Equal(t, count(f, "init "), 1)
	})
	t.Run("a deadline in the backoff", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		l.p.t.lxdBackoff = []time.Duration{time.Hour}
		require.NoError(t, l.prepareServer(context.Background()))
		resetOn(l, f, "init ", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := l.Start(ctx, Spec{Workdir: t.TempDir()})
		assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
	})
}

// a bridge already there is adopted when it is this run's.
func TestLXDBridgeAdopted(t *testing.T) {
	for _, owner := range []string{"mine", "theirs"} {
		t.Run(owner, func(t *testing.T) {
			l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
			require.NoError(t, l.prepareServer(context.Background()))
			run := l.p.token
			if owner == "theirs" {
				run = "another-run"
			}
			inner := f.run
			l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
				j := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(j, "network create"):
					f.record(args)
					io.WriteString(stderr, "Error: The network already exists")
					return &fakeExit{}
				case strings.HasPrefix(j, "query srv:/1.0/networks/patsbr"):
					f.record(args)
					io.WriteString(stdout, `{"config":{"user.pats.run":"`+run+`"}}`)
					return nil
				}
				return inner(ctx, stdin, stdout, stderr, args...)
			})
			sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir()})
			if owner == "mine" {
				require.NoError(t, err)
				sess.Close()
				return
			}
			require.Error(t, err, "already exists")
		})
	}
}

// a transient failure at the proxy's start removes both instances before
// the next try.
func TestLXDStartRetriedProxy(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	l, f := fakeLXD(t, Options{Driver: "lxd", ProxyBin: bin, Uplink: "up0"}, SandboxOptions{Image: "img", ProxyImage: "pimg"})
	require.NoError(t, l.prepareServer(context.Background()))
	answers := map[string]string{
		"egress-canary":   "egress-canary leak=0\n",
		"egress-reach":    "egress-reach ok=1\n",
		"id -u patsproxy": "999\n",
	}
	starts := 0
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ") + " "
		if strings.HasPrefix(joined, "start ") && strings.Contains(joined, "-p ") {
			if starts++; starts == 1 {
				f.record(args)
				io.WriteString(stderr, "Error: write: broken pipe")
				return &fakeExit{}
			}
		}
		for k, v := range answers {
			if strings.Contains(joined, k) {
				f.record(args)
				io.WriteString(stdout, v)
				return nil
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, Policy: proxy.Policy{Allow: proxy.Lines("example.com")}}})
	require.NoError(t, err)
	s := sess.(*lxdSession)
	assert.That(t, count(f, "delete srv:"+s.name+"-p ") >= 1, "the proxy instance was not removed")
	assert.That(t, count(f, "delete srv:"+s.name+" ") >= 1, "the instance was not removed")
	assert.Equal(t, count(f, "init "), 4) // two tries, each the sandbox and its proxy
	sess.Close()
}

// server setup: a failure is not kept, a create whose answer was lost and
// meets "already exists" is the earlier try's, and a failed setup leaves
// the marker.
func TestLXDSetupRetried(t *testing.T) {
	t.Run("a failure is not kept", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		resetOn(l, f, "profile device get", 0)
		require.Error(t, l.prepareServer(context.Background()), "transient")
		l.r = runFunc(f.run)
		require.NoError(t, l.prepareServer(context.Background()))
		assert.That(t, count(f, "network acl delete srv:pats-run-") == 0, "a failed setup removed its marker")
	})
	t.Run("a lost answer", func(t *testing.T) {
		l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
		creates := 0
		inner := f.run
		l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			if strings.HasPrefix(strings.Join(args, " "), "network acl create srv:pats-run-") {
				f.record(args)
				if creates++; creates == 1 {
					io.WriteString(stderr, "Error: read: connection reset by peer")
				} else {
					io.WriteString(stderr, "Error: The network ACL already exists")
				}
				return &fakeExit{}
			}
			return inner(ctx, stdin, stdout, stderr, args...)
		})
		require.NoError(t, l.prepareServer(context.Background()))
		assert.Equal(t, creates, 2)
		assert.Equal(t, count(f, "network acl show"), 0)
	})
}

// only a "not found" is a cache miss: a lookup that fails builds nothing.
func TestLXDImageLookupFails(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Uplink: "up0"})
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		j := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(j, "image list ubuntu: 26.04"):
			io.WriteString(stdout, `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`)
			return nil
		case j == "query srv:/1.0":
			io.WriteString(stdout, `{"environment":{"architectures":["x86_64"]}}`)
			return nil
		case strings.HasPrefix(j, "query srv:/1.0/images/aliases/pats-img-"):
			f.record(args)
			io.WriteString(stderr, "Error: read: connection reset by peer")
			return &fakeExit{}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	_, _, _, _, err := l.image(context.Background(), lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}, io.Discard)
	require.Error(t, err, "transient")
	assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
	assert.Equal(t, count(f, "launch "), 0)
}

// a doubtful key whose private alias is there is claimed, not built again;
// one whose lookup fails stays doubtful and builds nothing; one with neither
// alias is built once its old builder is removed.
func TestLXDImageReconcile(t *testing.T) {
	for _, c := range []string{"claimed", "lookup fails", "built"} {
		t.Run(c, func(t *testing.T) {
			l, f := fakeLXD(t, Options{Driver: "lxd", Uplink: "up0"})
			require.NoError(t, l.prepareServer(context.Background()))
			spec := lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}
			base := "abc"
			key, err := imageKey(base, spec, false)
			require.NoError(t, err)
			alias := "pats-img-" + key
			l.setDoubtful(alias, true)
			inner := f.run
			l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
				j := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(j, "image list ubuntu: 26.04"):
					io.WriteString(stdout, `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`)
					return nil
				case j == "query srv:/1.0":
					io.WriteString(stdout, `{"environment":{"architectures":["x86_64"]}}`)
					return nil
				case strings.HasPrefix(j, "query srv:/1.0/images/aliases/"+alias+"-"+l.p.token):
					f.record(args)
					switch c {
					case "claimed":
						io.WriteString(stdout, `{"target":"def"}`)
						return nil
					case "lookup fails":
						io.WriteString(stderr, "Error: read: connection reset by peer")
						return &fakeExit{}
					}
					io.WriteString(stderr, "Error: Image alias not found")
					return &fakeExit{}
				case strings.HasPrefix(j, "query srv:/1.0/images/aliases/"+alias):
					f.record(args)
					io.WriteString(stderr, "Error: Image alias not found")
					return &fakeExit{}
				case strings.HasPrefix(j, "query srv:/1.0/images/aliases/pats-img-"):
					f.record(args)
					io.WriteString(stdout, `{"target":"def"}`)
					return nil
				case strings.HasPrefix(j, "image alias create"):
					f.record(args)
					return nil
				}
				return inner(ctx, stdin, stdout, stderr, args...)
			})
			_, fp, _, _, err := l.image(context.Background(), spec, io.Discard)
			switch c {
			case "claimed":
				require.NoError(t, err)
				assert.Equal(t, fp, "def")
				assert.Equal(t, count(f, "launch "), 0)
				assert.That(t, !l.doubtful(alias), "still doubtful")
			case "lookup fails":
				require.Error(t, err, "transient")
				assert.Equal(t, count(f, "launch "), 0)
				assert.That(t, l.doubtful(alias), "no longer doubtful")
			case "built":
				assert.That(t, count(f, "delete srv:"+l.builderName(alias)) >= 1, "the old builder was not removed")
				assert.Equal(t, count(f, "launch "), 1)
			}
		})
	}
}

// a cache file that fails to import, and an alias check after it that fails
// too, builds nothing: the alias may be there.
func TestLXDImageCacheCheckFails(t *testing.T) {
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}}
	key := keyOf(t, spec)
	dir := t.TempDir()
	cacheFile(t, dir, "pats-img-"+key+".tar.zst", "one", map[string]string{"pats.inputs": "other"})
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{ImageCache: dir})
	lookups := 0
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if strings.HasPrefix(strings.Join(args, " "), "query srv:/1.0/images/aliases/pats-img-"+key) {
			if lookups++; lookups > 1 {
				io.WriteString(stderr, "Error: read: connection reset by peer")
				return &fakeExit{}
			}
		}
		return f.run(ctx, stdin, stdout, stderr, args...)
	})
	_, _, _, _, err := l.image(context.Background(), spec, io.Discard)
	require.Error(t, err, "the alias could not be checked")
	assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
	assert.Len(t, calls(f, "launch"), 0)
}

// a forced build that fails on a lost link leaves no doubtful mark.
func TestLXDForcedBuildNotDoubtful(t *testing.T) {
	f := newFakeServer(t)
	l := lxdOn(t, f, Options{Uplink: "up0"})
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		if args[0] == "launch" {
			io.WriteString(stderr, "Error: read: connection reset by peer")
			return &fakeExit{}
		}
		return f.run(ctx, stdin, stdout, stderr, args...)
	})
	spec := lxdImageSpec{Base: "img", Argv: []string{"true"}, Force: true}
	_, _, _, _, err := l.image(context.Background(), spec, io.Discard)
	require.Error(t, err, "transient")
	assert.That(t, !l.doubtful("pats-img-"+keyOf(t, spec)), "a forced build left a doubtful mark")
	spec.Force = false
	_, _, _, _, err = l.image(context.Background(), spec, io.Discard)
	require.Error(t, err, "transient")
	assert.That(t, l.doubtful("pats-img-"+keyOf(t, spec)), "a build that failed on a lost link is not doubtful")
}

// a transient failure inside the proxy image build ends the start: a build
// is not the pair's to try again.
func TestLXDStartProxyBuildNotRetried(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("elf"), 0o755))
	l, f := fakeLXD(t, Options{Driver: "lxd", ProxyBin: bin, Uplink: "up0"}, SandboxOptions{Image: "img"})
	require.NoError(t, l.prepareServer(context.Background()))
	answers := map[string]string{
		"egress-canary":   "egress-canary leak=0\n",
		"image list":      `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`,
		"query srv:/1.0 ": `{"environment":{"architectures":["x86_64"]}}`,
	}
	inner := f.run
	l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		joined := strings.Join(args, " ") + " "
		switch {
		case strings.HasPrefix(joined, "query srv:/1.0/images/aliases/"):
			f.record(args)
			io.WriteString(stderr, "Error: Image alias not found")
			return &fakeExit{}
		case strings.HasPrefix(joined, "launch "):
			f.record(args)
			io.WriteString(stderr, "Error: read: connection reset by peer")
			return &fakeExit{}
		}
		for k, v := range answers {
			if strings.Contains(joined, k) {
				f.record(args)
				io.WriteString(stdout, v)
				return nil
			}
		}
		return inner(ctx, stdin, stdout, stderr, args...)
	})
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{Set: true, Policy: proxy.Policy{Allow: proxy.Lines("example.com")}}})
	require.Error(t, err, "egress proxy image")
	assert.That(t, errors.Is(err, ErrPlace), "not ErrPlace: %v", err)
	assert.Equal(t, count(f, "init "), 1)
	assert.Equal(t, count(f, "launch "), 1)
}

// concurrent starts on a new server make one setup try at a time, and all
// see the one setup.
func TestLXDSetupConcurrent(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
	done := make(chan error, 4)
	for range 4 {
		go func() { done <- l.on(l.s).prepareServer(context.Background()) }()
	}
	for range 4 {
		require.NoError(t, <-done)
	}
	assert.Equal(t, count(f, "network acl create srv:pats-run-"), 1)
}

// a delete that lost the server is tried again though no operation runs.
func TestLXDDeleteInstanceTransient(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd", Remote: "srv"})
	resetOn(l, f, "delete srv:pats-x", 1)
	require.NoError(t, l.deleteGone("pats-x"))
	assert.Equal(t, count(f, "delete srv:pats-x"), 2)
}
