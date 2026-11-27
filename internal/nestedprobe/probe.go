// Package nestedprobe checks, from inside a sandbox, that the devices a
// nested lxd needs to run vms work: kvm runs a vcpu, vhost-vsock takes a
// guest cid, /dev/vsock opens. vhost-net is recorded, not required. it runs
// as root in the sandbox vm, as `pats nested-probe` (docs/spec/nested.md).
package nestedprobe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Version is the protocol the probe's output line speaks.
const Version = "1"

// prefix starts the one line pats reads from the probe.
const prefix = "pats-nested-probe"

// the verdicts of one check.
const (
	OK      = "ok"
	Missing = "missing" // no node
	Failed  = "failed"  // the node is there and the ioctls fail
	Unknown = "unknown" // the probe did not run
)

// Result is what the probe found, one verdict a check.
type Result struct {
	KVM           string `json:"kvm"`
	VhostVsockCID string `json:"vhost_vsock_cid"`
	Vsock         string `json:"vsock"`
	VhostNet      string `json:"vhost_net"`
	// Reason is each failed or missing check's error, in check order.
	Reason string `json:"reason,omitempty"`
}

// OK is whether every required check passed: vhost-net is not required.
func (r Result) OK() bool {
	return r.KVM == OK && r.VhostVsockCID == OK && r.Vsock == OK
}

// Unknowns is a Result for a probe that did not run.
func Unknowns() Result {
	return Result{KVM: Unknown, VhostVsockCID: Unknown, Vsock: Unknown, VhostNet: Unknown}
}

// Write prints r as the probe's line.
func Write(w io.Writer, r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s %s %s\n", prefix, Version, b)
	return err
}

// ErrMismatch is a probe output without a line of this version: the binary
// predates the probe or speaks another version of it.
type ErrMismatch struct{ Got string }

func (e *ErrMismatch) Error() string {
	if e.Got == "" {
		return "the probe binary does not match: it printed no " + prefix + " line"
	}
	return fmt.Sprintf("the probe binary does not match: it speaks version %s, pats reads %s", e.Got, Version)
}

// Parse reads the probe's line out of its output.
func Parse(out string) (Result, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var got string
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) < 3 || f[0] != prefix {
			continue
		}
		if f[1] != Version {
			got = f[1]
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(f[2]), &r); err != nil {
			return Result{}, fmt.Errorf("the probe's line does not parse: %w", err)
		}
		for _, v := range []string{r.KVM, r.VhostVsockCID, r.Vsock, r.VhostNet} {
			switch v {
			case OK, Missing, Failed:
			default:
				return Result{}, fmt.Errorf("the probe's line has verdict %q", v)
			}
		}
		return r, nil
	}
	return Result{}, &ErrMismatch{Got: got}
}

// Run makes every check, whatever an earlier one found.
func Run() Result {
	var r Result
	var why []string
	check := func(name string, f func() (string, error)) string {
		v, err := f()
		if err != nil {
			why = append(why, name+": "+err.Error())
		}
		return v
	}
	r.KVM = check("kvm", checkKVM)
	r.VhostVsockCID = check("vhost-vsock", checkVhostVsock)
	r.Vsock = check("vsock", checkVsock)
	r.VhostNet = check("vhost-net", checkVhostNet)
	r.Reason = strings.Join(why, "; ")
	return r
}
