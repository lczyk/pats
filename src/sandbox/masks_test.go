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

// an engine that maps uids elsewhere says so: rootless docker or a userns
// remap, rootless podman.
func TestRemapsUIDs(t *testing.T) {
	cases := map[string][]struct {
		info string
		want bool
	}{
		"docker": {{`["name=apparmor","name=seccomp,profile=builtin"]`, false}, {`["name=seccomp","name=rootless"]`, true}, {`["name=userns"]`, true}},
		"podman": {{"false", false}, {"true", true}},
	}
	for _, driver := range Engines() {
		for _, c := range cases[driver] {
			f := newFakeDaemon(t, driver)
			f.remapsInfo = c.info
			d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: driver})
			require.NoError(t, err)
			got, err := d.(interface {
				RemapsUIDs(context.Context) (bool, error)
			}).RemapsUIDs(context.Background())
			require.NoError(t, err)
			assert.Equal(t, got, c.want, "%s: %s", driver, c.info)
		}
	}
}

// a container, agent and egress sidecar alike, keeps no-new-privileges unless
// the daemon is the docker snap's, whose apparmor profile such a container
// cannot leave: asked once per driver, podman never.
func TestNoNewPrivsOffUnderConfinedDaemon(t *testing.T) {
	for _, c := range []struct {
		driver, root string
		want         bool
	}{
		{"docker", "/var/lib/docker", true},
		{"docker", "/var/snap/docker/common/var-lib-docker", false},
		{"podman", "/var/snap/docker/common/var-lib-docker", true},
	} {
		if !IsEngine(c.driver) {
			continue
		}
		for _, mode := range containerModes {
			t.Run(c.driver+c.root+"/"+mode.name, func(t *testing.T) {
				f := newFakeDaemon(t, c.driver)
				f.rootDir = c.root
				d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: c.driver})
				require.NoError(t, err)
				for range 2 {
					sb, err := d.Sandbox(SandboxOptions{Image: "img"})
					require.NoError(t, err)
					sess, err := sb.Start(context.Background(), Spec{Workdir: workdir(t), Egress: mode.egress})
					require.NoError(t, err)
					require.NoError(t, sess.Close())
				}
				runs, infos := 0, 0
				for _, cmd := range f.all() {
					if strings.HasPrefix(cmd, "run -d ") {
						runs++
						assert.Equal(t, strings.Contains(cmd, "--security-opt no-new-privileges"), c.want, cmd)
					}
					if strings.HasPrefix(cmd, "info --format {{.DockerRootDir}}") {
						infos++
					}
				}
				assert.That(t, runs >= 2, "runs: %d", runs)
				if c.driver == "docker" {
					assert.Equal(t, infos, 1)
				} else {
					assert.Equal(t, infos, 0)
				}
			})
		}
	}
}
