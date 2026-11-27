package nestedprobe

import (
	"os"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

func needKVM(t *testing.T) {
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skip("no usable /dev/kvm")
	} else {
		f.Close()
	}
}

// where kvm works, the vcpu runs to its exit and the payload checks out.
func TestCheckKVM(t *testing.T) {
	needKVM(t)
	v, err := checkKVM()
	assert.NoError(t, err)
	assert.Equal(t, v, OK)
}

// a vcpu that never exits is stopped at the timeout, a failure, and the
// probe goes on.
func TestCheckKVMTimeout(t *testing.T) {
	needKVM(t)
	code, timeout := guestCode, runTimeout
	defer func() { guestCode, runTimeout = code, timeout }()
	guestCode, runTimeout = spinCode, 300*time.Millisecond
	start := time.Now()
	v, err := checkKVM()
	assert.Equal(t, v, Failed)
	assert.Error(t, err, "did not exit in time")
	assert.That(t, time.Since(start) < 5*time.Second, "took %s", time.Since(start))
}
