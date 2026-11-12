package sandbox

import (
	"archive/tar"
	"bytes"
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
	// open is the driver on its fake; fetches counts the fake's pulls, nil
	// where the fake keeps no count.
	open func(t *testing.T) (d Driver, fetches func() int)
	// inUse starts something on image ref, or is nil where a place lets
	// an image in use go (lxd: an instance does not hold its image).
	inUse func(t *testing.T, d Driver, ref string)
	// noBuild is a driver that builds no images, and says so.
	noBuild bool
}

var driverCases = []driverCase{
	{name: "lxd", open: func(t *testing.T) (Driver, func() int) {
		f := newFakeServer(t)
		d, err := newDriver(testProcess(t), runFunc(f.run), Options{Driver: "lxd"})
		require.NoError(t, err)
		return d, func() int { return f.copies }
	}},
	containerDriverCase("docker"),
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
				if fetches != nil {
					assert.Equal(t, fetches(), 1)
				}

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
				if c.noBuild {
					_, _, _, err := d.Build(ctx, spec, io.Discard)
					assert.Error(t, err, "builds no images")
					return
				}
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

	needDriver(t, "podman")
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
	// harness clis go in over the base and under the provision step, each
	// from a build context of its own. the comment is the tree's identity,
	// so a pin or a bin that moves changes the dockerfile, and the build.
	claude := Artefact{CLI: "claude", Version: "2.1.319", Arch: "arm64", URL: "https://a.example/x", SHA256: "aa", Bin: "package/claude"}
	codex := Artefact{CLI: "codex", Version: "0.167.0", Arch: "arm64", SHA256: "bb", Bin: "package/bin/codex"}
	df := provisionDockerfile(BuildSpec{Base: "ubuntu:26.04", Material: "/m", Argv: []string{"./x.sh"},
		Harness: []Placed{{Artefact: claude}, {Artefact: codex}}}, nil)
	assert.Equal(t, df, "ARG PATS_SANDBOX\nFROM ubuntu:26.04\nUSER root\n"+
		"# claude 2.1.319 arm64 aa package/claude\nCOPY --chown=0:0 --from=pats-harness-claude / /\n"+
		"# codex 0.167.0 arm64 bb package/bin/codex\nCOPY --chown=0:0 --from=pats-harness-codex / /\n"+
		"COPY . /pats-provision\n"+
		`RUN ["/bin/sh","-c","PATH=\"/pats-provision/bin:$PATH\"; export PATH; exec \"$@\"","provision","./x.sh"]`+"\n"+
		"RUN rm -rf /pats-provision\n")
	moved := claude
	moved.URL = "https://mirror.example/x"
	assert.Equal(t, provisionDockerfile(BuildSpec{Base: "b", Harness: []Placed{{Artefact: moved}}}, nil),
		provisionDockerfile(BuildSpec{Base: "b", Harness: []Placed{{Artefact: claude}}}, nil)) // a url is not in the key
	rebinned := claude
	rebinned.Bin = "package/bin/claude"
	assert.That(t, provisionDockerfile(BuildSpec{Base: "b", Harness: []Placed{{Artefact: rebinned}}}, nil) !=
		provisionDockerfile(BuildSpec{Base: "b", Harness: []Placed{{Artefact: claude}}}, nil), "a bin is in the key")
}

// a real build: a cli's tree lands in the image over its base, root's, on
// PATH, and its stamp is what the image says. a second build of the same
// inputs is the first's image, with no tree unpacked.
func TestContainerBuildPlacesHarness(t *testing.T) {
	dockerOrSkip(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := tarGz(t,
		tarEntry{name: "package/", mode: 0o755, typ: tar.TypeDir},
		tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\necho 'tool 1.2.3'\n"},
	)
	a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "arm64", SHA256: "aa", Bin: "package/tool"}
	d, err := Open(Options{Driver: "docker"})
	require.NoError(t, err)
	ctx := context.Background()
	_, _, err = d.Pull(ctx, testImage, false, io.Discard)
	require.NoError(t, err)
	arch, err := d.Arch(ctx, testImage)
	require.NoError(t, err)
	assert.That(t, arch == "amd64" || arch == "arm64", "arch %q", arch)

	spec := BuildSpec{Base: testImage, Harness: []Placed{{Artefact: a, File: file}}}
	img, _, built, err := d.Build(ctx, spec, io.Discard) // a failed build's error carries its tail
	require.NoError(t, err)
	assert.Equal(t, built, true)
	t.Cleanup(func() { _ = d.RemoveImage(context.Background(), img.ID) })

	stamps, err := d.Stamps(ctx, img.ID, []string{"tool", "absent"})
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"tool": "1.2.3"})

	sb, err := d.Sandbox(SandboxOptions{Image: img.ID})
	require.NoError(t, err)
	var out, errb bytes.Buffer
	code, err := sb.Run(ctx, Spec{Argv: []string{"sh", "-c", "command -v tool; stat -c '%u:%g %a' /opt/pats/harness/tool/package/tool; readlink /usr/local/bin/tool"}, Workdir: t.TempDir()}, &out, &errb)
	require.NoError(t, err, "%s", errb.String())
	assert.Equal(t, code, 0, errb.String())
	assert.Equal(t, out.String(), "/usr/local/bin/tool\n0:0 755\n/opt/pats/harness/tool/package/tool\n")

	again, _, built, err := d.Build(ctx, BuildSpec{Base: testImage, Harness: []Placed{{Artefact: a, File: "/nonexistent/not-read"}}}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, built, false)
	assert.Equal(t, again.ID, img.ID)
}

// podman's overrides reach the cli the driver runs, not only its target.
func TestEngineRunnerEnv(t *testing.T) {
	assert.Equal(t, engineRunner(Options{Driver: "docker", ContainerHost: "x"}).(execRunner).env == nil, true)
	needDriver(t, "podman")
	r := engineRunner(Options{Driver: "podman", ContainerHost: "ssh://box/run/podman.sock", ContainerConnection: "c"}).(execRunner)
	assert.EqualArrays(t, r.env, []string{"CONTAINER_HOST=ssh://box/run/podman.sock", "CONTAINER_CONNECTION=c"})
	assert.Equal(t, execRunner{bin: "podman"}.environ() == nil, true)
}
