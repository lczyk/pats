package sandbox

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the driver conformance table: what every Driver promises of its images
// and its identity, run against each driver's fake, beside the session one.

type driverCase struct {
	name string
	open func(t *testing.T) (d Driver, fetches func() int)
	// inUse starts something on image ref, or is nil where a place lets
	// an image in use go (lxd: an instance does not hold its image).
	inUse func(t *testing.T, d Driver, ref string)
}

var driverCases = []driverCase{
	{name: "lxd", open: func(t *testing.T) (Driver, func() int) {
		f := newFakeServer(t)
		d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: "lxd"})
		require.NoError(t, err)
		return d, func() int { return f.copies }
	}},
	containerDriverCase("docker"),
	containerDriverCase("podman"),
}

func containerDriverCase(driver string) driverCase {
	return driverCase{name: driver,
		open: func(t *testing.T) (Driver, func() int) {
			f := newFakeDaemon(t, driver)
			d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: driver})
			require.NoError(t, err)
			return d, func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.pulls }
		},
		inUse: func(t *testing.T, d Driver, ref string) {
			sb, err := d.Sandbox(SandboxOptions{Image: ref})
			require.NoError(t, err)
			sess, err := sb.Start(context.Background(), Spec{Workdir: workdir(t)})
			require.NoError(t, err)
			t.Cleanup(func() { sess.Close() })
		},
	}
}

func TestDriverConformance(t *testing.T) {
	ctx := context.Background()
	for _, c := range driverCases {
		t.Run(c.name, func(t *testing.T) {
			t.Run("pull, then resolve", func(t *testing.T) {
				d, fetches := c.open(t)
				img, fetched, err := d.Pull(ctx, "ubuntu:26.04", false, io.Discard)
				require.NoError(t, err)
				assert.That(t, fetched, "the first pull fetched nothing")
				got, err := d.Resolve(ctx, "ubuntu:26.04")
				require.NoError(t, err)
				assert.Equal(t, got.ID, img.ID)
				ids, err := d.Images(ctx)
				require.NoError(t, err)
				assert.That(t, ids[img.ID], "Images lacks the pulled image")

				again, fetched, err := d.Pull(ctx, "ubuntu:26.04", false, io.Discard)
				require.NoError(t, err)
				assert.That(t, !fetched, "a second pull fetched")
				assert.Equal(t, again.ID, img.ID)
				assert.Equal(t, fetches(), 1)

				_, fetched, err = d.Pull(ctx, "ubuntu:26.04", true, io.Discard)
				require.NoError(t, err)
				assert.That(t, fetched, "a forced pull fetched nothing")
			})
			t.Run("a missing image", func(t *testing.T) {
				d, _ := c.open(t)
				_, err := d.Resolve(ctx, "ubuntu:99.99")
				assert.That(t, errors.Is(err, ErrNoImage), "a missing ref is not ErrNoImage: %v", err)
				_, err = d.Resolve(ctx, "pats-img-0123456789ab")
				assert.That(t, errors.Is(err, ErrNoImage), "a missing local ref is not ErrNoImage: %v", err)
			})
			t.Run("remove", func(t *testing.T) {
				d, _ := c.open(t)
				img, _, err := d.Pull(ctx, "ubuntu:26.04", false, io.Discard)
				require.NoError(t, err)
				require.NoError(t, d.RemoveImage(ctx, img.ID))
				ids, err := d.Images(ctx)
				require.NoError(t, err)
				assert.That(t, !ids[img.ID], "Images still lists a removed image")
				assert.NoError(t, d.RemoveImage(ctx, img.ID)) // gone already
			})
			if c.inUse != nil {
				t.Run("an image in use stays", func(t *testing.T) {
					d, _ := c.open(t)
					img, _, err := d.Pull(ctx, "ubuntu:26.04", false, io.Discard)
					require.NoError(t, err)
					c.inUse(t, d, "ubuntu:26.04")
					assert.Error(t, d.RemoveImage(ctx, img.ID), "")
					_, err = d.Resolve(ctx, img.ID)
					assert.NoError(t, err)
				})
			}
			t.Run("build, then resolve", func(t *testing.T) {
				d, _ := c.open(t)
				spec := BuildSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}
				img, _, built, err := d.Build(ctx, spec, io.Discard)
				require.NoError(t, err)
				assert.That(t, built, "the first build did not build")
				got, err := d.Resolve(ctx, img.ID)
				require.NoError(t, err)
				assert.Equal(t, got.ID, img.ID)
				again, _, built, err := d.Build(ctx, spec, io.Discard)
				require.NoError(t, err)
				assert.That(t, !built, "identical inputs built again")
				assert.Equal(t, again.ID, img.ID)
			})
			t.Run("target", func(t *testing.T) {
				d, _ := c.open(t)
				a, err := d.Target(ctx)
				require.NoError(t, err)
				b, err := d.Target(ctx)
				require.NoError(t, err)
				assert.Equal(t, a, b)
				assert.That(t, a.Key != "", "no pin key")
				assert.That(t, d.CPUs(ctx) > 0, "no cpus")
			})
		})
	}
}

// the target names the place as the cli resolves it now: docker's context
// and daemon, podman's overrides first, then its default connection.
func TestContainerTarget(t *testing.T) {
	ctx := context.Background()
	open := func(o Options, conns ...map[string]any) Driver {
		f := newFakeDaemon(t, o.Driver)
		f.conns = conns
		d, err := newDriver(testProcess(t), runFunc(f.run), o)
		require.NoError(t, err)
		return d
	}
	got, err := open(Options{Driver: "docker"}).Target(ctx)
	require.NoError(t, err)
	assert.Equal(t, got, Target{Key: "docker:default", Daemon: "DAEMON:docker", Version: "27.1.1"})

	got, err = open(Options{Driver: "podman", ContainerHost: "ssh://core@box/run/podman.sock"}).Target(ctx)
	require.NoError(t, err)
	assert.Equal(t, got, Target{Key: "podman:ssh://core@box/run/podman.sock", Daemon: "ssh://core@box/run/podman.sock"})

	conns := []map[string]any{{"Name": "a", "URI": "ssh://a"}, {"Name": "b", "URI": "ssh://b", "Default": true}}
	got, err = open(Options{Driver: "podman"}, conns...).Target(ctx)
	require.NoError(t, err)
	assert.Equal(t, got, Target{Key: "podman:b", Daemon: "ssh://b"})

	got, err = open(Options{Driver: "podman", ContainerConnection: "a"}, conns...).Target(ctx)
	require.NoError(t, err)
	assert.Equal(t, got, Target{Key: "podman:a", Daemon: "ssh://a"})

	_, err = open(Options{Driver: "podman", ContainerConnection: "c"}, conns...).Target(ctx)
	assert.Error(t, err, `podman connection "c" (CONTAINER_CONNECTION) not found`)

	got, err = open(Options{Driver: "podman"}).Target(ctx)
	require.NoError(t, err)
	assert.Equal(t, got, Target{Key: "podman:local", Daemon: "local"})
}

// an image pulled from two repositories has a digest in each: Resolve
// gives the one of the repository asked for, and an id the first.
func TestContainerResolveDigest(t *testing.T) {
	ctx := context.Background()
	f := newFakeDaemon(t, "docker")
	d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: "docker"})
	require.NoError(t, err)
	a, _, err := d.Pull(ctx, "python:3", false, nil)
	require.NoError(t, err)
	b, _, err := d.Pull(ctx, "other/python:3", false, nil)
	require.NoError(t, err)
	require.Equal(t, a.ID, b.ID)
	got, err := d.Resolve(ctx, "other/python:3")
	require.NoError(t, err)
	assert.Equal(t, got.Digest, "sha256:d-other")
	got, err = d.Resolve(ctx, "python:3")
	require.NoError(t, err)
	assert.Equal(t, got.Digest, "sha256:d-python")
	got, err = d.Resolve(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Repo+"@"+got.Digest, Repository("python:3")+"@sha256:d-python")
}

// a provision build runs its step on its base, with the material copied in
// for the step and removed after; with no base, on the pats sandbox.
func TestProvisionDockerfile(t *testing.T) {
	assert.Equal(t, provisionDockerfile(BuildSpec{Base: "ubuntu:26.04", Material: "/m", Argv: []string{"./x.sh"}}, nil),
		"ARG PATS_SANDBOX\nFROM ubuntu:26.04\nUSER root\nCOPY . /pats-provision\n"+
			`RUN ["/bin/sh","-c","PATH=\"/pats-provision/bin:$PATH\"; export PATH; exec \"$@\"","provision","./x.sh"]`+"\n"+
			"RUN rm -rf /pats-provision\n")
	assert.Equal(t, provisionDockerfile(BuildSpec{}, func() string { return "pats/sandbox:v1" }),
		"ARG PATS_SANDBOX=pats/sandbox:v1\nFROM ${PATS_SANDBOX}\nUSER root\n")
}

// podman's overrides reach the cli the driver runs, not only its target.
func TestEngineRunnerEnv(t *testing.T) {
	r := engineRunner(Options{Driver: "podman", ContainerHost: "ssh://box/run/podman.sock", ContainerConnection: "c"}).(execRunner)
	assert.EqualArrays(t, r.env, []string{"CONTAINER_HOST=ssh://box/run/podman.sock", "CONTAINER_CONNECTION=c"})
	assert.Equal(t, engineRunner(Options{Driver: "docker", ContainerHost: "x"}).(execRunner).env == nil, true)
	assert.Equal(t, execRunner{bin: "podman"}.environ() == nil, true)
}
