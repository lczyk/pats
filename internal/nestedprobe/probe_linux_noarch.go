//go:build linux && !amd64 && !arm64

package nestedprobe

import (
	"errors"
	"runtime"
)

var guestCode, spinCode []byte

func setupVcpu(_, _ int) error {
	return errors.New("no vcpu probe for " + runtime.GOARCH)
}

func checkExit([]byte) error { return nil }
