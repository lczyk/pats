package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/nestedprobe"
)

func probeLine(t *testing.T, r nestedprobe.Result) string {
	var b strings.Builder
	require.NoError(t, nestedprobe.Write(&b, r))
	return b.String()
}

var allOK = nestedprobe.Result{KVM: "ok", VhostVsockCID: "ok", Vsock: "ok", VhostNet: "missing"}

// a vm sandbox with nested: vms gets the probe as root after boot and
// before the workdir push, its binary pushed and then removed.
func TestLXDNestedVMsProbe(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	l, f := fakeLXD(t, Options{Driver: "lxd", VM: true, ProxyBin: bin}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedVMs, Required: true}})
	f.uname, f.probeOut = "x86_64", "noise\n"+probeLine(t, allOK)
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	defer sess.Close()
	r := sess.(NestedSession).Nested()
	require.NotNil(t, r)
	assert.That(t, r.OK() != nil && *r.OK(), "want ok, got %+v", r)
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "--vm")
	assert.That(t, !strings.Contains(strings.Join(f.call("init"), " "), "security.nesting"), "security.nesting on a vm")

	calls := f.all()
	at := func(sub string) int {
		return slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, sub) })
	}
	probe, push, rm, work := at(lxdProbeDir+"/pats nested-probe"), at("push "+lxdProbeDir), at("rm -f "+lxdProbeDir+"/pats"), at(" "+WorkMount+" ")
	assert.That(t, push >= 0 && push < probe && probe < rm, "push %d, probe %d, rm %d", push, probe, rm)
	assert.That(t, work < 0 || rm < work, "the probe ran after the workdir push: %d > %d", rm, work)
}

// a failed required probe is a launch error carrying the result; not
// required, the pair starts and records it.
func TestLXDNestedVMsProbeFails(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, nil, 0o755))
	bad := nestedprobe.Result{KVM: "missing", VhostVsockCID: "ok", Vsock: "ok", VhostNet: "ok", Reason: "kvm: no /dev/kvm"}
	for _, required := range []bool{true, false} {
		l, f := fakeLXD(t, Options{Driver: "lxd", VM: true, ProxyBin: bin}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedVMs, Required: required}})
		f.uname, f.probeOut = "x86_64", probeLine(t, bad)
		sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
		if !required {
			require.NoError(t, err)
			r := sess.(NestedSession).Nested()
			assert.That(t, r.OK() != nil && !*r.OK(), "want ok false, got %+v", r)
			sess.Close()
			continue
		}
		var ne *NestedError
		require.That(t, errors.As(err, &ne), "want a NestedError, got %v", err)
		assert.That(t, errors.Is(err, ErrLaunch), "not a launch error: %v", err)
		assert.ContainsString(t, err.Error(), "no /dev/kvm")
		assert.ContainsString(t, err.Error(), "kvm_intel.nested")
		assert.That(t, slices.ContainsFunc(f.all(), func(c string) bool { return strings.Contains(c, "rm -f "+lxdProbeDir+"/pats") }), "the probe binary was not removed")
	}
}

// a probe binary that prints no line of this version is told apart from a
// failed check, and the probe did not run: ok is null.
func TestLXDNestedVMsProbeMismatch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, nil, 0o755))
	for _, c := range []struct {
		out  string
		code int
	}{{"error: unknown command nested-probe\n", 0}, {"pats-nested-probe 0 {}\n", 0}, {"Unknown command `nested-probe'\n", 1}} {
		l, f := fakeLXD(t, Options{Driver: "lxd", VM: true, ProxyBin: bin}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedVMs, Required: true}})
		f.uname, f.probeOut, f.probeCode = "x86_64", c.out, c.code
		_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
		var ne *NestedError
		require.That(t, errors.As(err, &ne), "want a NestedError, got %v", err)
		assert.That(t, ne.Result.OK() == nil, "ok is not null")
		assert.ContainsString(t, err.Error(), "does not match")
		assert.ContainsString(t, err.Error(), LXDProxyBinEnv)
		assert.ContainsString(t, err.Error(), "from "+bin)
	}
	// a vm of an arch pats has no build for never fetches.
	l, f := fakeLXD(t, Options{Driver: "lxd", VM: true, ProxyBin: bin}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedVMs, Required: true}})
	f.uname = "riscv64"
	_, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "riscv64")
	assert.That(t, !slices.ContainsFunc(f.all(), func(c string) bool { return strings.Contains(c, lxdProbeDir) }), "pushed a probe for an unknown arch")
}

// containers on a container sandbox is security.nesting, set by pats unless
// the config already sets it; vms there is refused; on a vm, nothing to set.
func TestLXDNestedContainers(t *testing.T) {
	l, f := fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedContainers, Required: true}})
	sess, err := l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.ContainsString(t, strings.Join(f.call("init"), " "), "-c security.nesting=true")
	assert.Equal(t, sess.(NestedSession).Nested().Set, "security.nesting")
	sess.Close()

	l, f = fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img", Config: map[string]string{"security.nesting": "true"}, Nested: Nested{Run: NestedContainers}})
	sess, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.Equal(t, strings.Count(strings.Join(f.call("init"), " "), "security.nesting"), 1)
	sess.Close()

	l, f = fakeLXD(t, Options{Driver: "lxd", VM: true}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedContainers}})
	sess, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.Equal(t, sess.(NestedSession).Nested().Set, "")
	assert.Nil(t, f.call("exec srv:"+sess.(*lxdSession).name+" --project pats --force-noninteractive --user 0 --group 0 --cwd / -- uname"))
	sess.Close()

	l, f = fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img", Nested: Nested{Run: NestedVMs}})
	_, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	assert.Error(t, err, "needs vm: true")
	assert.Nil(t, f.call("init"))

	l, _ = fakeLXD(t, Options{Driver: "lxd"}, SandboxOptions{Image: "img"})
	sess, err = l.Start(context.Background(), Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
	require.NoError(t, err)
	assert.Nil(t, sess.(NestedSession).Nested())
	sess.Close()
}

// a vm builder for a sandbox with nested: vms runs under the sandbox's
// limits and is probed before the provision step: a failed probe stops a
// required build there, and is a warning otherwise.
func TestLXDImageBuildNested(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pats")
	require.NoError(t, os.WriteFile(bin, nil, 0o755))
	bad := nestedprobe.Result{KVM: "failed", VhostVsockCID: "ok", Vsock: "ok", VhostNet: "ok", Reason: "kvm: KVM_RUN: exit reason 8"}
	run := func(t *testing.T, probe nestedprobe.Result, required bool) (*fakeLXC, []string, []string, error) {
		l, f := fakeLXD(t, Options{Driver: "lxd", VM: true, Uplink: "up0", ProxyBin: bin})
		f.uname, f.probeOut = "aarch64", probeLine(t, probe)
		inner := f.run
		l.r = runFunc(func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
			joined := strings.Join(args, " ")
			switch {
			case joined == "query srv:/1.0":
				f.record(args)
				io.WriteString(stdout, `{"environment":{"architectures":["aarch64"]}}`)
				return nil
			case strings.HasPrefix(joined, "image list ubuntu: 26.04"):
				f.record(args)
				io.WriteString(stdout, `[{"fingerprint":"abc","aliases":[{"name":"26.04"}]}]`)
				return nil
			case strings.HasPrefix(joined, "query srv:/1.0/images/aliases/pats-img-"):
				f.record(args)
				if strings.Contains(joined, "-"+l.p.token+"?") {
					io.WriteString(stdout, `{"target":"def"}`)
					return nil
				}
				io.WriteString(stderr, "Error: Image alias not found")
				return &fakeExit{}
			}
			return inner(ctx, stdin, stdout, stderr, args...)
		})
		var steps, warns []string
		spec := lxdImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"},
			Limits: Limits{CPU: 4, Memory: "6GiB", Disk: "30GiB"}, Nested: Nested{Run: NestedVMs, Required: required},
			Progress: func(m string) { steps = append(steps, m) }, Warn: func(m string) { warns = append(warns, m) }}
		_, _, _, _, err := l.image(context.Background(), spec, io.Discard)
		return f, steps, warns, err
	}
	provisioned := func(f *fakeLXC) bool {
		return slices.ContainsFunc(f.all(), func(c string) bool { return strings.Contains(c, "provision true") })
	}

	f, steps, _, err := run(t, allOK, true)
	require.NoError(t, err)
	launch := strings.Join(f.call("launch"), " ")
	for _, want := range []string{"--vm", "limits.cpu=4", "limits.memory=6GiB", "root,size=30GiB"} {
		assert.ContainsString(t, launch, want)
	}
	probe := slices.IndexFunc(steps, func(s string) bool { return s == "probing nested vms" })
	prov := slices.IndexFunc(steps, func(s string) bool { return strings.HasPrefix(s, "provisioning") })
	assert.That(t, probe >= 0 && probe < prov, "steps %q", steps)

	f, _, _, err = run(t, bad, true)
	var ne *NestedError
	require.That(t, errors.As(err, &ne), "want a NestedError, got %v", err)
	assert.ContainsString(t, err.Error(), "cannot run a vcpu")
	assert.That(t, !provisioned(f), "provisioned after a failed probe")

	f, _, warns, err := run(t, bad, false)
	require.NoError(t, err)
	assert.That(t, provisioned(f), "not provisioned")
	assert.That(t, slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "building anyway") }), "warns %q", warns)
}
