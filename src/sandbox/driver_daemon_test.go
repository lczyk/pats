package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// onDaemon is a container driver on the real driver cli, with a process of
// its own, or a skip when the daemon is not there.
func onDaemon(t *testing.T, driver string) *container {
	t.Helper()
	if _, err := exec.LookPath(driver); err != nil {
		t.Skip(driver + " not on PATH")
	}
	if err := exec.Command(driver, "info").Run(); err != nil {
		t.Skip(driver + " not responding")
	}
	d, err := newDriver(testProcess(t), nil, Options{Driver: driver})
	require.NoError(t, err)
	return d.(*container)
}

// what the fakes model of the image methods, on a real docker and podman:
// pull and resolve, the target, a base / provision build and its cache, and
// removal of what was built.
func TestContainerImagesOnDaemon(t *testing.T) {
	ctx := context.Background()
	ref := "docker.io/library/" + testImage
	for _, driver := range []string{"docker", "podman"} {
		t.Run(driver, func(t *testing.T) {
			c := onDaemon(t, driver)
			img, _, err := c.Pull(ctx, ref, false, io.Discard)
			require.NoError(t, err)
			got, err := c.Resolve(ctx, ref)
			require.NoError(t, err)
			assert.Equal(t, got.ID, img.ID)
			assert.That(t, strings.HasPrefix(got.Digest, "sha256:"), "no registry digest: %+v", got)
			byID, err := c.Resolve(ctx, img.ID)
			require.NoError(t, err)
			assert.Equal(t, byID.Digest, got.Digest)
			ids, err := c.Images(ctx)
			require.NoError(t, err)
			assert.That(t, ids[img.ID], "Images lacks %s", img.ID)
			_, err = c.Resolve(ctx, "docker.io/library/pats-no-such-image:0")
			assert.That(t, errorsIsNoImage(err), "a missing image is not ErrNoImage: %v", err)

			target, err := c.Target(ctx)
			require.NoError(t, err)
			assert.That(t, strings.HasPrefix(target.Key, driver+":"), "target %+v", target)
			assert.That(t, c.CPUs(ctx) > 0, "no cpus")

			material := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(material, "bin"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(material, "bin", "mark"), []byte("#!/bin/sh\necho \"$1\" > /pats-mark\n"), 0o755))
			spec := BuildSpec{Base: ref, Material: material, Argv: []string{"mark", t.Name() + time.Now().String()}}
			built, _, ran, err := c.Build(ctx, spec, io.Discard)
			require.NoError(t, err)
			assert.That(t, ran, "the first build did not build")
			t.Cleanup(func() { _ = c.RemoveImage(context.Background(), built.ID) })
			again, _, ran, err := c.Build(ctx, spec, io.Discard)
			require.NoError(t, err)
			assert.That(t, !ran, "identical inputs built again")
			assert.Equal(t, again.ID, built.ID)

			sb, err := c.Sandbox(SandboxOptions{Image: built.ID})
			require.NoError(t, err)
			sess, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			require.NoError(t, err)
			var out strings.Builder
			code, err := sess.Exec(ctx, Cmd{Argv: []string{"sh", "-c", "cat /pats-mark; test ! -e " + ProvisionMount}, Root: true}, &out, &out)
			require.NoError(t, err)
			assert.Equal(t, code, 0, "%s", out.String())
			assert.ContainsString(t, out.String(), t.Name())
			assert.Error(t, c.RemoveImage(ctx, built.ID), "") // a container uses it
			require.NoError(t, sess.Close())
			require.NoError(t, c.RemoveImage(ctx, built.ID))
			require.NoError(t, c.RemoveImage(ctx, built.ID)) // gone already
		})
	}
}

// a run's containers and networks on a real docker and podman: listed by
// their labels, left while the run's lease is fresh, taken once it is stale.
func TestContainerCleanOnDaemon(t *testing.T) {
	ctx := context.Background()
	for _, driver := range []string{"docker", "podman"} {
		t.Run(driver, func(t *testing.T) {
			run := onDaemon(t, driver)
			sb, err := run.Sandbox(SandboxOptions{Image: "docker.io/library/" + testImage})
			require.NoError(t, err)
			sess, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: Egress{None: true}})
			require.NoError(t, err)
			t.Cleanup(func() { sess.Close() })
			pool, err := parsePool("")
			require.NoError(t, err)
			net := "sbx-egr-cleantest-" + run.p.token
			_, err = run.createNetwork(ctx, net, pool)
			require.NoError(t, err)
			t.Cleanup(func() { _ = exec.Command(driver, "network", "rm", net).Run() })
			crashLease(run.p)

			runs, err := run.Runs(ctx)
			require.NoError(t, err)
			var mine *RunRecord
			for i := range runs {
				if runs[i].Token == run.p.token {
					mine = &runs[i]
				}
			}
			require.That(t, mine != nil, "the run is not listed: %+v", runs)
			assert.Equal(t, mine.Owner, run.p.owner())
			kinds := map[string]int{}
			for _, r := range mine.Resources {
				kinds[r.Kind]++
				assert.That(t, time.Since(r.Created) < time.Hour, "%s %s created %s", r.Kind, r.Name, r.Created)
			}
			assert.EqualMaps(t, kinds, map[string]int{"sandbox": 1, "network": 1})

			cleaner := onDaemon(t, driver)
			cleaner.p.home = run.p.home
			_, err = cleaner.p.clean(ctx, cleaner, CleanOptions{Place: driver})
			require.NoError(t, err)
			assert.That(t, labelled(t, driver, run.p.token) != "", "a live run was taken")

			cleaner.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
			var out strings.Builder
			_, err = cleaner.p.clean(ctx, cleaner, CleanOptions{Place: driver, Report: TextReport(&out)})
			require.NoError(t, err)
			assert.Equal(t, labelled(t, driver, run.p.token), "", "a dead run's leftovers stayed:\n%s", out.String())
		})
	}
}

// labelled is every container and network carrying run token's label.
func labelled(t *testing.T, driver, token string) string {
	t.Helper()
	ctrs, err := exec.Command(driver, "ps", "-a", "-q", "--filter", "label=pats.run="+token).Output()
	require.NoError(t, err)
	nets, err := exec.Command(driver, "network", "ls", "-q", "--filter", "label=pats.run="+token).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(ctrs) + string(nets))
}

func errorsIsNoImage(err error) bool { return errors.Is(err, ErrNoImage) }

// a proxied session on a real docker and podman, with every egress mode's
// helpers (the ca read, the canary and reach probes, the sidecar): every
// container and network it makes carries the run's labels, Close leaves
// none, and a run that crashed mid-session is taken by a clean once its
// lease is stale. PATS_E2E_EGRESS_IMAGE names a proxy image built from
// this tree (images/Dockerfile.egress-proxy) on both engines.
func TestContainerEgressCleanOnDaemon(t *testing.T) {
	img := os.Getenv("PATS_E2E_EGRESS_IMAGE")
	if img == "" {
		t.Skip("set PATS_E2E_EGRESS_IMAGE to a proxy image built from this tree")
	}
	ctx := context.Background()
	for _, driver := range []string{"docker", "podman"} {
		for _, mode := range lxdEgressModes[2:] {
			t.Run(driver+"/"+mode.name, func(t *testing.T) {
				eg := mode.egress
				eg.Image = img
				// a trust bundle in the image, as the pats sandbox has: ubuntu
				// ships none, and docker desktop alone puts one in.
				base := onDaemon(t, driver)
				material := t.TempDir()
				cert, _, err := GenCA()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(material, "bundle.pem"), cert, 0o644))
				withCA, _, _, err := base.Build(ctx, BuildSpec{Base: "docker.io/library/" + testImage, Material: material,
					Argv: []string{"sh", "-c", "mkdir -p /etc/ssl/certs && cp " + ProvisionMount + "/bundle.pem /etc/ssl/certs/ca-certificates.crt"}}, io.Discard)
				require.NoError(t, err)
				t.Cleanup(func() { _ = base.RemoveImage(context.Background(), withCA.ID) })
				start := func() (*container, Session) {
					c := onDaemon(t, driver)
					sb, err := c.Sandbox(SandboxOptions{Image: withCA.ID})
					require.NoError(t, err)
					sess, err := sb.Start(ctx, Spec{Workdir: t.TempDir(), Egress: eg})
					require.NoError(t, err)
					return c, sess
				}
				c, sess := start()
				runs, err := c.Runs(ctx)
				require.NoError(t, err)
				kinds := map[string]int{}
				for _, r := range runs {
					if r.Token == c.p.token {
						for _, res := range r.Resources {
							kinds[res.Kind]++
						}
					}
				}
				assert.EqualMaps(t, kinds, map[string]int{"sandbox": 2, "network": 1}) // agent and sidecar
				require.NoError(t, sess.Close())
				assert.Equal(t, labelled(t, driver, c.p.token), "", "Close left some")

				dead, _ := start()
				crashLease(dead.p)
				cleaner := onDaemon(t, driver)
				cleaner.p.home = dead.p.home
				cleaner.p.clock = fixedClock{now: time.Now().Add(time.Hour)}
				var out strings.Builder
				_, err = cleaner.p.clean(ctx, cleaner, CleanOptions{Place: driver, Report: TextReport(&out)})
				require.NoError(t, err)
				assert.Equal(t, labelled(t, driver, dead.p.token), "", "a crashed run's leftovers stayed:\n%s", out.String())
			})
		}
	}
}
