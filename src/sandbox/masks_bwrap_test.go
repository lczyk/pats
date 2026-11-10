//go:build !nobwrap

package sandbox

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

// under bwrap a mask is a tmpfs right after the bind it sits under.
func TestBwrapMountMasks(t *testing.T) {
	s := &bwrapSession{rootfs: "/r", dir: "/d", workdir: "/w",
		binds: []bind{{host: "/repo", path: "/repo", ro: true, masks: []string{"/repo/.pats"}}, {host: "/out", path: "/repo/.pats/runs/a/t"}}}
	argv := strings.Join(s.argv(Cmd{Argv: []string{"true"}}), " ")
	assert.ContainsString(t, argv, "--ro-bind /repo /repo --tmpfs /repo/.pats --bind /out /repo/.pats/runs/a/t")
}
