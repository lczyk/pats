package nestedprobe

import (
	"errors"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the line round-trips among other output; another version, no line, or a
// verdict outside the set is not a result.
func TestParse(t *testing.T) {
	in := Result{KVM: OK, VhostVsockCID: Failed, Vsock: OK, VhostNet: Missing, Reason: "vhost-vsock: VHOST_VSOCK_SET_GUEST_CID 7: busy"}
	var b strings.Builder
	require.NoError(t, Write(&b, in))
	got, err := Parse("warning: x\n" + b.String() + "trailer\n")
	require.NoError(t, err)
	assert.Equal(t, got, in)
	assert.That(t, !got.OK(), "a failed vhost-vsock is ok")
	in.VhostVsockCID, in.VhostNet = OK, Failed
	assert.That(t, in.OK(), "vhost-net is not required")

	var mm *ErrMismatch
	_, err = Parse("unknown command `nested-probe'\n")
	require.That(t, errors.As(err, &mm), "want a mismatch, got %v", err)
	assert.Equal(t, mm.Got, "")
	_, err = Parse(prefix + " 2 {}\n")
	require.That(t, errors.As(err, &mm), "want a mismatch, got %v", err)
	assert.Equal(t, mm.Got, "2")
	_, err = Parse(prefix + " " + Version + ` {"kvm":"unknown","vhost_vsock_cid":"ok","vsock":"ok","vhost_net":"ok"}` + "\n")
	assert.Error(t, err, `verdict "unknown"`)
	_, err = Parse(prefix + " " + Version + " {\n")
	assert.Error(t, err, "does not parse")
}

// every check runs whatever an earlier one found, and the reason names
// each failure.
func TestRunMakesEveryCheck(t *testing.T) {
	r := Run()
	for _, v := range []string{r.KVM, r.VhostVsockCID, r.Vsock, r.VhostNet} {
		assert.That(t, v == OK || v == Missing || v == Failed, "verdict %q", v)
	}
	if !r.OK() {
		assert.That(t, r.Reason != "", "a failure without a reason")
	}
}
