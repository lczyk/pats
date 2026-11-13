package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a docker that snapd linked, wherever its bin dir is, and anything in
// /snap/bin, is the docker snap's cli; an apt one is not.
func TestSnapCLI(t *testing.T) {
	dir := t.TempDir()
	linked := filepath.Join(dir, "docker")
	require.NoError(t, os.Symlink("/usr/bin/snap", linked))
	apt := filepath.Join(dir, "apt-docker")
	require.NoError(t, os.WriteFile(apt, nil, 0o755))
	elsewhere := filepath.Join(dir, "other")
	require.NoError(t, os.Symlink("/usr/bin/docker", elsewhere))
	noLink := func(string) (string, error) { return "", errors.New("not a link") }
	snapLink := func(string) (string, error) { return "/usr/bin/snap", nil }

	assert.That(t, snapCLI(linked, os.Readlink), "a link to /usr/bin/snap")
	assert.That(t, snapCLI("/snap/bin/docker", snapLink), "a link in /snap/bin")
	assert.That(t, snapCLI("/var/lib/snapd/snap/bin/docker", snapLink), "a link in /var/lib/snapd/snap/bin")
	assert.That(t, snapCLI("/snap/bin/docker", noLink), "in /snap/bin and no link")
	assert.That(t, !snapCLI(apt, os.Readlink), "an apt docker")
	assert.That(t, !snapCLI(elsewhere, os.Readlink), "a link to another docker")

	// a link of the user's to the snap's docker, by one step or two, absolute
	// or relative; a link that goes round ends the walk.
	chain := map[string]string{
		"/usr/local/bin/docker":          "/snap/bin/docker",
		"/opt/bin/docker":                "../../usr/local/bin/docker",
		"/opt/bin/snapd-docker":          "/var/lib/snapd/snap/bin/docker",
		"/var/lib/snapd/snap/bin/docker": "/usr/bin/snap",
		"/opt/bin/apt-docker":            "/usr/bin/docker",
		"/opt/bin/loop":                  "/opt/bin/loop",
	}
	walk := func(p string) (string, error) {
		if t, ok := chain[p]; ok {
			return t, nil
		}
		return "", errors.New("not a link")
	}
	for _, p := range []string{"/usr/local/bin/docker", "/opt/bin/docker", "/opt/bin/snapd-docker"} {
		assert.That(t, snapCLI(p, walk), "%s leads to the snap's docker", p)
	}
	for _, p := range []string{"/opt/bin/apt-docker", "/opt/bin/loop"} {
		assert.That(t, !snapCLI(p, walk), "%s does not", p)
	}
}

// snapProcess is a test process whose docker is the docker snap's cli.
func snapProcess(t *testing.T) *process {
	p := testProcess(t)
	p.dockerSnap = func() bool { return true }
	return p
}

// with the docker snap, a dir for docker's cli is in the client's dir of the
// docker snap's data, owned as any temp dir, with a link left in the temp
// root; another driver's is in the temp root, and so is docker's without the
// snap.
func TestMkTempFor(t *testing.T) {
	p := snapProcess(t)
	home, _ := p.home()
	root := p.cliRoot(p.owner())
	assert.Equal(t, filepath.Dir(root), p.snapData())

	dir, err := p.mkTempFor("docker", "pats-cid-")
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(dir), root)
	b, err := os.ReadFile(dir + ownerSuffix)
	require.NoError(t, err)
	assert.Equal(t, string(b), p.token)
	fi, err := os.Stat(root)
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o700))
	link := filepath.Join(home, "tmp", cliRootLink)
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, got, root)

	for _, driver := range []string{"podman", "lxd", "bwrap", "host", ""} {
		d, err := p.mkTempFor(driver, "pats-x-")
		require.NoError(t, err)
		assert.Equal(t, filepath.Dir(d), filepath.Join(home, "tmp"), "driver %q", driver)
	}

	// the last dir gone, the client's dir goes; the link stays, and a second
	// dir leaves it as it is.
	require.NoError(t, RemoveTemp(dir))
	_, err = os.Stat(dir + ownerSuffix)
	assert.That(t, os.IsNotExist(err), "an owner file stayed")
	_, err = os.Stat(root)
	assert.That(t, os.IsNotExist(err), "an empty cli root stayed")
	_, err = os.Lstat(link)
	assert.NoError(t, err)
	dir, err = p.mkTempFor("docker", "pats-cid-")
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(dir), root)
	got, _ = os.Readlink(link)
	assert.Equal(t, got, root)

	plain := testProcess(t)
	ph, _ := plain.home()
	d, err := plain.mkTempFor("docker", "pats-cid-")
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(d), filepath.Join(ph, "tmp"))
	_, err = os.Lstat(filepath.Join(ph, "tmp", cliRootLink))
	assert.That(t, os.IsNotExist(err), "a link with no docker snap")
	_, err = os.Stat(plain.snapData())
	assert.That(t, os.IsNotExist(err), "the docker snap's dir made with no docker snap")
}

// what the docker snap's cli opens is under the cli root: the cidfile, the
// iidfile, the generated dockerfile and a placed cli's tree. a bind's source
// is not. nothing of a start or a build stays behind.
func TestCLIRootPaths(t *testing.T) {
	for _, driver := range []string{"docker", "podman"} {
		t.Run(driver, func(t *testing.T) {
			f := newFakeDaemon(t, driver)
			p := snapProcess(t)
			home, _ := p.home()
			want := filepath.Join(home, "tmp")
			if driver == "docker" {
				want = p.cliRoot(p.owner())
			}
			under := func(path string) bool { return within(want, path) }
			c := newContainer(p, runFunc(f.run), Options{Driver: driver}).on(SandboxOptions{Image: "img"})
			work, err := p.mkTemp("pats-work-")
			require.NoError(t, err)
			sess, err := c.Start(context.Background(), Spec{Workdir: work})
			require.NoError(t, err)
			defer sess.Close()
			run := f.call("run -d")
			require.NotNil(t, run)
			assert.That(t, under(flagValue(run, "--cidfile")), "the cidfile: %v", run)
			assert.Equal(t, flagValue(run, "-v"), work+":"+WorkMount)
			assert.That(t, within(filepath.Join(home, "tmp"), work), "a bind's source left the temp root")

			file := tarGz(t,
				tarEntry{name: "package/", mode: 0o755, typ: tar.TypeDir},
				tarEntry{name: "package/tool", mode: 0o755, typ: tar.TypeReg, body: "#!/bin/sh\necho 'tool 1.2.3'\n"},
			)
			a := Artefact{CLI: "tool", Version: "1.2.3", Arch: "arm64", SHA256: "aa", Bin: "package/tool"}
			_, _, _, err = c.Build(context.Background(), BuildSpec{Base: "ubuntu:26.04", Argv: []string{"true"},
				Harness: []Placed{{Artefact: a, File: file}}}, io.Discard)
			require.NoError(t, err)
			build := f.call("build")
			require.NotNil(t, build)
			assert.That(t, under(flagValue(build, "--iidfile")), "the iidfile: %v", build)
			assert.That(t, under(flagValue(build, "-f")), "the dockerfile: %v", build)
			_, tree, _ := strings.Cut(flagValue(build, "--build-context"), "=")
			assert.That(t, under(tree), "a placed cli's tree: %v", build)
			assert.That(t, under(build[len(build)-1]), "the empty context: %v", build)

			ents, _ := os.ReadDir(want)
			var left []string
			for _, e := range ents {
				if n := e.Name(); !strings.HasPrefix(n, "pats-work-") && n != cliRootLink {
					left = append(left, n)
				}
			}
			assert.Equal(t, len(left), 0, "left behind: %v", left)
		})
	}
}

// within says whether p is dir or under it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return p != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// flagValue is the argument after the first flag in args, "" with none.
func flagValue(args []string, flag string) string {
	for i, a := range args[:len(args)-1] {
		if a == flag {
			return args[i+1]
		}
	}
	return ""
}

// clean sweeps the client's dir in the docker snap's data by the temp root's
// rules, whatever docker is on PATH now: a dead run's dir goes, a live one's
// and another client's stay, and a lease stays while a dir there names it.
func TestCleanCLIRoot(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	p := testProcess(t) // the cleaner: no docker snap on its PATH
	mk := func(t *testing.T) (*process, string) {
		run := snapProcess(t)
		run.home, run.snapData = p.home, p.snapData
		dir, err := run.mkTempFor("docker", "pats-material-")
		require.NoError(t, err)
		require.NoError(t, os.Chtimes(dir, old, old))
		return run, dir
	}
	live, liveDir := mk(t)
	dead, deadDir := mk(t)
	root := filepath.Dir(deadDir)
	other := filepath.Join(p.snapData(), "another-client", "pats-material-1")
	require.NoError(t, os.MkdirAll(other, 0o700))
	require.NoError(t, os.Chtimes(other, old, old))

	// the dead run's lease, stale: it stays while its dir names it.
	crashLease(dead)
	leasePath, _ := dead.leasePath(dead.token)
	stale := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(leasePath, stale, stale))

	var out bytes.Buffer
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, true, false, TextReport(&out)))
	assert.ContainsString(t, out.String(), "would remove temp dir "+deadDir+"\n")
	assert.That(t, !strings.Contains(out.String(), "lease"), "a dry run would take a lease a dir names: %s", out.String())
	_, err := os.Stat(deadDir)
	assert.NoError(t, err)

	out.Reset()
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, false, false, TextReport(&out)))
	_, err = os.Stat(deadDir)
	assert.That(t, os.IsNotExist(err), "a dead run's dir stayed")
	_, err = os.Stat(leasePath)
	assert.NoError(t, err) // read before its dir went
	for _, kept := range []string{liveDir, other} {
		_, err = os.Stat(kept)
		assert.NoError(t, err)
	}
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 24*time.Hour, false, false, TextReport(&out)))
	_, err = os.Stat(leasePath)
	assert.That(t, os.IsNotExist(err), "the lease outlived its dirs")

	// the link is no dir: clean leaves it, and the root once the live run is done.
	home, _ := p.home()
	_, err = os.Lstat(filepath.Join(home, "tmp", cliRootLink))
	assert.NoError(t, err)
	require.NoError(t, RemoveTemp(liveDir))
	_ = live
	_, err = os.Stat(root)
	assert.That(t, os.IsNotExist(err), "an empty cli root stayed")
}

// a clean with no docker snap dir makes none, and one that may not read it
// (the pats snap, of another snap's data) says nothing.
func TestCleanCLIRootAbsent(t *testing.T) {
	p := testProcess(t)
	var out bytes.Buffer
	for _, dry := range []bool{true, false} {
		require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 0, dry, false, TextReport(&out)))
	}
	_, err := os.Stat(p.snapData())
	assert.That(t, os.IsNotExist(err), "clean made the docker snap's dir")
	home, _ := p.home()
	_, err = os.Stat(filepath.Join(home, "tmp"))
	assert.That(t, os.IsNotExist(err), "clean made the temp root")

	if os.Getuid() == 0 {
		t.Skip("root reads any dir")
	}
	root := p.cliRoot(p.owner())
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pats-x-1"), 0o700))
	require.NoError(t, os.Chmod(root, 0))
	defer os.Chmod(root, 0o700)
	require.NoError(t, p.cleanTemp(map[string]bool{}, nil, 0, false, true, TextReport(&out)))
	assert.Equal(t, out.String(), "")
}
