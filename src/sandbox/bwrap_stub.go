//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

func newBwrap() (Sandbox, error) {
	return nil, fmt.Errorf("sandbox driver bwrap is linux-only (this is %s)", runtime.GOOS)
}

// Run is unreachable (newBwrap never constructs one here) but keeps the type
// satisfying Sandbox on all platforms.
func (b *bwrapSandbox) Run(context.Context, Spec, io.Writer, io.Writer) (int, error) {
	return -1, errors.New("bwrap: not supported on this platform")
}

// NetnsMain is the linux-only `pats __sbx-net` helper entrypoint.
func NetnsMain([]string) int {
	fmt.Fprintln(os.Stderr, "__sbx-net: linux-only")
	return 1
}
