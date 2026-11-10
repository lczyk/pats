package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a mask is a tmpfs over its path after the mount it sits under, and the
// keepalive runs as root whatever the image's USER.
func TestContainerMountMasks(t *testing.T) {
	for _, driver := range Engines() {
		t.Run(driver, func(t *testing.T) {
			f := newFakeDaemon(t, driver)
			d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: driver})
			require.NoError(t, err)
			sb, err := d.Sandbox(SandboxOptions{Image: "img"})
			require.NoError(t, err)
			tree := t.TempDir()
			sess, err := sb.Start(context.Background(), Spec{Workdir: workdir(t), Egress: Egress{None: true},
				Mounts: []Mount{{Host: tree, Container: tree, ReadOnly: true, Masks: []string{tree + "/.pats"}}}})
			require.NoError(t, err)
			defer sess.Close()
			var run string
			for _, c := range f.all() {
				if strings.HasPrefix(c, "run -d ") {
					run = c
				}
			}
			assert.ContainsString(t, run, "--user 0:0")
			mask := "--mount type=tmpfs,destination=" + tree + "/.pats" + engines[driver].maskOpts
			assert.ContainsString(t, run, "-v "+tree+":"+tree+":ro "+mask)
		})
	}
}
