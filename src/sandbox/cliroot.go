package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// the cli root is where pats makes the dirs the docker cli opens itself: a
// cidfile's, an iidfile's, a generated dockerfile's, a placed cli's tree, a
// step's material. it is the temp root, but for the docker snap's cli, which
// has a /tmp of its own and sees no hidden dir at the top of the home: there
// it is a dir of the docker snap's own data. a bind mount's source stays in
// the temp root, since the daemon resolves it. see
// docs/spec/docker-snap-paths.md.

// cliRootName is the dir pats keeps in the docker snap's data, one dir per
// client id under it.
const cliRootName = "pats-tmp"

// cliRootLink names the link in the temp root that points at the cli root.
const cliRootLink = "docker-snap"

// MkTempFor is MkTemp for a dir the cli of a sandbox of driver opens.
func MkTempFor(driver, prefix string) (string, error) {
	return defaultProcess.mkTempFor(driver, prefix)
}

func (p *process) mkTempFor(driver, prefix string) (string, error) {
	if driver != "docker" || !p.dockerSnap() {
		return p.mkTemp(prefix)
	}
	root := p.cliRoot(p.owner())
	if root == "" {
		return p.mkTemp(prefix)
	}
	if err := p.holdLease(); err != nil {
		return "", err
	}
	// a RemoveTemp that emptied the root takes it: made again, at most twice.
	var dir string
	var err error
	for try := 0; try < 3; try++ {
		if err = os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		if dir, err = os.MkdirTemp(root, prefix); !errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dir+ownerSuffix, []byte(p.token), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	p.linkCLIRoot(root)
	return dir, nil
}

// cliRoot is the docker snap's cli root of the client owner names
// (host/uid/client); "" with no client id or no home.
func (p *process) cliRoot(owner string) string {
	base := p.snapData()
	id := owner[strings.LastIndexByte(owner, '/')+1:]
	if base == "" || id == "" {
		return ""
	}
	return filepath.Join(base, id)
}

// linkCLIRoot leaves a link to root in the temp root, for whoever looks for
// a run's dirs there. the cli is never handed the link: it cannot pass
// through a hidden dir.
func (p *process) linkCLIRoot(root string) {
	home, err := p.home()
	if err != nil {
		return
	}
	tmp := filepath.Join(home, "tmp")
	if os.MkdirAll(tmp, 0o700) == nil {
		_ = os.Symlink(root, filepath.Join(tmp, cliRootLink))
	}
}

// inCLIRoot says whether dir was made in a docker snap cli root.
func inCLIRoot(dir string) bool {
	return filepath.Base(filepath.Dir(filepath.Dir(dir))) == cliRootName
}

// snapDockerData is where pats keeps its dirs in the docker snap's data, in
// the user's own home (a strict snap moves $HOME away from it).
func snapDockerData() string {
	home := os.Getenv("SNAP_REAL_HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "snap", "docker", "common", cliRootName)
}

// DockerSnapCLI says whether the docker pats runs is the docker snap's cli.
func DockerSnapCLI() bool { return defaultProcess.dockerSnap() }

// dockerSnapCLI looks the docker on PATH up, once per process.
func dockerSnapCLI() bool {
	path, err := exec.LookPath("docker")
	return err == nil && snapCLI(path, os.Readlink)
}

// snapCLI says whether the binary at path is a snap's: a link to
// /usr/bin/snap, as snapd makes them (in /snap/bin, or /var/lib/snapd/snap/bin
// on a distro with no /snap), or anything in /snap/bin. the link is read one
// step and not resolved, since /usr/bin/snap may itself be a link.
func snapCLI(path string, readlink func(string) (string, error)) bool {
	if filepath.Dir(path) == "/snap/bin" {
		return true
	}
	target, err := readlink(path)
	return err == nil && target == "/usr/bin/snap"
}
