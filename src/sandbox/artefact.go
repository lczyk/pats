package sandbox

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Artefact is one file a harness cli is published as, for one arch: where it
// is fetched from, what its bytes must hash to, and how it is placed
// (docs/spec/harness-images.md).
type Artefact struct {
	CLI     string // the executable's name on PATH
	Version string
	Arch    string
	URL     string
	SHA256  string
	Bin     string // the executable inside the file, a gzipped tar; "" when the file is the executable
}

// maxArtefact is the largest artefact pats takes, in bytes: 2 GiB. a var for
// the test of the limit.
var maxArtefact int64 = 2 << 30

// HarnessTreeDir is where a cli's files sit in an image, and HarnessBinDir
// where its name is linked from.
const (
	HarnessTreeDir = "opt/pats/harness"
	HarnessBinDir  = "usr/local/bin"
)

// TreeID names what a cli's tree holds: its cli, version, artefact sha and
// bin, everything that decides its contents and none of its bytes.
func (a Artefact) TreeID() string {
	sum := sha256.Sum256([]byte(a.Line()))
	return hex.EncodeToString(sum[:])
}

// Line is the artefact as an image's key takes it: a corrected bin is another
// tree, a moved url is not.
func (a Artefact) Line() string {
	return strings.Join([]string{a.CLI, a.Version, a.Arch, a.SHA256, a.Bin}, " ")
}

// artefactCache is the directory the fetched artefacts are kept in, one file
// each named by its sha256, shared by every project, sandbox and driver on
// the machine.
func artefactCache() (string, error) {
	c, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "harness"), nil
}

// FetchArtefact returns the cached file of a, fetching it first when the
// cache has none, or one whose bytes no longer hash to a's sha256. what it
// returns has just been hashed. a fetch whose bytes do not match is an error
// naming both shas, and keeps nothing. client nil is http.DefaultClient,
// which follows redirects and takes the machine's proxy from the environment.
func FetchArtefact(ctx context.Context, a Artefact, client *http.Client) (string, error) {
	dir, err := artefactCache()
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, a.SHA256)
	// a cached file that no longer matches is fetched once more, and the
	// fetch renamed over it: never removed first, which could take away the
	// file another process just put there.
	switch sum, err := hashFile(file); {
	case err == nil && sum == a.SHA256:
		return file, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	tmp, got, err := download(ctx, a, client, dir)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp) // a no-op once it is renamed into place
	if got != a.SHA256 {
		return "", fmt.Errorf("%s: sha256 is %s, the pin says %s", a.what(), got, a.SHA256)
	}
	if err := os.Rename(tmp, file); err != nil {
		return "", err
	}
	return file, nil
}

// PinArtefact fetches a, whose sha256 is not known yet, and returns what its
// bytes hash to: trust on first use. the file is kept in the cache under
// that sha, so an init that pins it fetches nothing.
func PinArtefact(ctx context.Context, a Artefact, client *http.Client) (string, error) {
	dir, err := artefactCache()
	if err != nil {
		return "", err
	}
	tmp, got, err := download(ctx, a, client, dir)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	return got, os.Rename(tmp, filepath.Join(dir, got))
}

// what names an artefact in an error.
func (a Artefact) what() string {
	return fmt.Sprintf("%s %s for %s (%s)", a.CLI, a.Version, a.Arch, a.URL)
}

// download fetches a's url into a temp file in dir, hashing as it writes,
// and returns the file and its sha256. the caller removes or renames it.
func download(ctx context.Context, a Artefact, client *http.Client, dir string) (tmp, sum string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", a.what(), err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", a.what(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s: the source answered %s", a.what(), resp.Status)
	}
	f, err := os.CreateTemp(dir, "fetch-*")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArtefact+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxArtefact {
		err = fmt.Errorf("more than %d bytes, refused", maxArtefact)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("%s: %w", a.what(), err)
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile is the sha256 of a file's bytes.
func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyFileBytes(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildTree unpacks an artefact's file into dir as the tree its cli is placed
// from, the same under every driver: opt/pats/harness/<cli>/ holding the
// unpacked artefact (or the file itself as <cli>), and usr/local/bin/<cli>
// linking to the executable in it. the artefact is not pats's own: it is
// unpacked by the rules a registry's layer is (unpack.go), so no entry leaves
// the tree, devices are skipped and setuid bits dropped.
func BuildTree(a Artefact, file, dir string) error {
	if !cliNameRe.MatchString(a.CLI) {
		return fmt.Errorf("%q is not a cli's name", a.CLI)
	}
	base := filepath.Join(dir, filepath.FromSlash(HarnessTreeDir), a.CLI)
	// afresh: what an earlier artefact of the cli left is no part of this one.
	if err := os.RemoveAll(base); err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	exe := a.CLI
	if a.Bin == "" {
		if err := copyExecutable(file, filepath.Join(base, exe)); err != nil {
			return err
		}
	} else {
		exe = a.Bin
		if err := unpackTarGz(file, base); err != nil {
			return fmt.Errorf("%s %s: %w", a.CLI, a.Version, err)
		}
		// looked for through a root of the tree: a link that leads out of
		// it (to this machine's /bin/sh) is not the artefact's executable.
		root, err := os.OpenRoot(base)
		if err != nil {
			return err
		}
		fi, err := root.Stat(filepath.FromSlash(exe))
		root.Close()
		switch {
		case err != nil:
			return fmt.Errorf("%s %s: bin %s is not in the artefact", a.CLI, a.Version, a.Bin)
		case !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0:
			return fmt.Errorf("%s %s: bin %s is not an executable file", a.CLI, a.Version, a.Bin)
		}
	}
	// what this machine's umask took from the modes is given back: the tree
	// is root's in the image, and its user has to read and run it.
	top := filepath.Join(dir, filepath.FromSlash(HarnessTreeDir))
	if err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		perm := fi.Mode().Perm()
		return os.Chmod(p, perm|(perm&0o500)>>3|(perm&0o500)>>6)
	}); err != nil {
		return err
	}
	bin := filepath.Join(dir, filepath.FromSlash(HarnessBinDir))
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	// the directories above the two are an image's own (/, /usr, /opt), and
	// a copy may give them the tree's modes: never this machine's umask.
	for _, rel := range []string{HarnessTreeDir, HarnessBinDir} {
		for p := filepath.Join(dir, filepath.FromSlash(rel)); strings.HasPrefix(p, dir); p = filepath.Dir(p) {
			if err := os.Chmod(p, 0o755); err != nil {
				return err
			}
			if p == dir {
				break
			}
		}
	}
	link := filepath.Join(bin, a.CLI)
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// absolute, as the image sees it.
	return os.Symlink("/"+HarnessTreeDir+"/"+a.CLI+"/"+exe, link)
}

func unpackTarGz(file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return applyLayer(gz, root, false)
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err = copyFileBytes(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755) // whatever the umask left
}
