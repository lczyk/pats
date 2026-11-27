package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/internal/nestedprobe"
)

// what a sandbox lets the agent start inside it (docs/spec/nested.md).
const (
	NestedNone       = ""
	NestedContainers = "containers"
	NestedVMs        = "vms"
)

// Nested is a sandbox's nested declaration: what may run inside, and
// whether a sandbox that cannot is a launch error or a recorded degradation.
type Nested struct {
	Run      string // NestedNone | NestedContainers | NestedVMs
	Required bool
}

// NestedResult is what a session found of its nested declaration, as the
// pair's metadata records it.
type NestedResult struct {
	Asked    string
	Required bool
	// Set is what pats set for containers ("security.nesting"), "" when
	// nothing (a vm). containers only.
	Set string
	// Probed is whether the probe ran to its line; Probe holds it then.
	// vms only.
	Probed bool
	Probe  nestedprobe.Result
	// Reason is why the probe failed or could not run.
	Reason string
}

// OK is the probe's verdict: nil when it did not run or nothing was probed.
func (r *NestedResult) OK() *bool {
	if r.Asked != NestedVMs || !r.Probed {
		return nil
	}
	ok := r.Probe.OK()
	return &ok
}

// NestedError is a start that failed its required nested check. it carries
// the result for the pair's metadata; the start wraps it in ErrLaunch.
type NestedError struct {
	Result *NestedResult
}

func (e *NestedError) Error() string { return nestedMessage(e.Result) }

// NestedSession is a session that records a nested declaration.
type NestedSession interface {
	Nested() *NestedResult // nil when nothing was asked
}

// nestedMessage is a failed nested check in pats's words, by what failed.
func nestedMessage(r *NestedResult) string {
	if !r.Probed {
		return "nested: vms, but the probe could not run: " + r.Reason
	}
	p := r.Probe
	var why []string
	switch p.KVM {
	case nestedprobe.Missing:
		why = append(why, "the vm has no /dev/kvm: is nested virtualisation on in the lxd server's host kernel (kvm_intel.nested / kvm_amd.nested on amd64)?")
	case nestedprobe.Failed:
		why = append(why, "kvm in the vm cannot run a vcpu (on amd64 the vmx / svm cpu flag must reach the vm; on arm64 the kernel and cpu generation must support nesting)")
	}
	if p.VhostVsockCID != nestedprobe.OK {
		why = append(why, "vhost-vsock in the vm cannot take a guest: the nested lxd would refuse to start vms")
	}
	if p.Vsock != nestedprobe.OK {
		why = append(why, "the vm has no /dev/vsock: the nested lxd refuses to start vms without it (is the virtio vsock transport in the image?)")
	}
	msg := "nested: vms, but " + strings.Join(why, "; and ")
	if r.Reason != "" {
		msg += " (" + r.Reason + ")"
	}
	return msg + " (docs/spec/nested.md)"
}

// lxdProbeDir is where the probe binary goes in the instance, for its run.
const lxdProbeDir = "/usr/local/lib/pats"

// unameArch maps uname -m to a go arch.
func unameArch(m string) (string, error) {
	switch m {
	case "x86_64":
		return "amd64", nil
	case "aarch64":
		return "arm64", nil
	}
	return "", fmt.Errorf("the vm's architecture %q has no pats build", m)
}

// probeNested runs `pats nested-probe` as root in the vm: its binary is
// pushed for the vm's own arch, run, and removed on every path after the
// push.
func (s *lxdSession) probeNested(ctx context.Context, n Nested) *NestedResult {
	r := &NestedResult{Asked: NestedVMs, Required: n.Required}
	fail := func(err error) *NestedResult {
		r.Reason = err.Error()
		return r
	}
	var out bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: []string{"uname", "-m"}, Root: true, Dir: "/"}, &out, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("uname -m exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fail(err)
	}
	arch, err := unameArch(strings.TrimSpace(out.String()))
	if err != nil {
		return fail(err)
	}
	dir, err := s.l.p.mkTemp("pats-probe-")
	if err != nil {
		return fail(err)
	}
	defer RemoveTemp(dir)
	bin := filepath.Join(dir, "pats")
	if err := proxyBinary(ctx, s.l.p.reg, s.l.o.ProxyBin, arch, bin, s.l.p.t.proxyFetch); err != nil {
		return fail(fmt.Errorf("the probe binary: %w", err))
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fail(err)
	}
	dst := lxdProbeDir + "/pats"
	defer func() {
		// cut short (a cancel), the file stays in an instance deleted with
		// the pair.
		bg, done := context.WithTimeout(context.WithoutCancel(ctx), s.l.p.t.cliCleanup)
		defer done()
		_, _ = s.exec(bg, Cmd{Argv: []string{"rm", "-f", dst}, Root: true, Dir: "/"}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	if err := s.Push(ctx, bin, dst); err != nil {
		return fail(fmt.Errorf("push the probe: %w", err))
	}
	var stdout, stderr bytes.Buffer
	code, err = s.exec(ctx, Cmd{Argv: []string{dst, "nested-probe"}, Root: true, Dir: "/"}, &stdout, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("exited %d: %s", code, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	if err != nil {
		return fail(fmt.Errorf("run the probe: %w", err))
	}
	res, err := nestedprobe.Parse(stdout.String())
	if err != nil {
		var mm *nestedprobe.ErrMismatch
		if errors.As(err, &mm) {
			err = fmt.Errorf("%w (set %s to a matching linux pats)", err, LXDProxyBinEnv)
		}
		return fail(err)
	}
	r.Probed, r.Probe, r.Reason = true, res, res.Reason
	return r
}

// Nested is what the session found of its nested declaration.
func (s *lxdSession) Nested() *NestedResult { return s.nested }
