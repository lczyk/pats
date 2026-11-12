//go:build !nobwrap

package sandbox

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

// the bwrap driver's images have no daemon to hold them: pats fetches a
// public image's layers over the registry's http api and unpacks them into
// a store under its cache, one rootfs per image, named by the digest of the
// image's manifest for this machine's architecture.
//
//	<cache>/bwrap/rootfs/<id>/        the unpacked rootfs
//	<cache>/bwrap/rootfs/<id>.new/    an unpack in progress, or a crashed one
//	<cache>/bwrap/rootfs/<id>.lock    held exclusively by an unpack, shared by a session
//	<cache>/bwrap/rootfs/<id>.env.json   the image config's Env
//	<cache>/bwrap/rootfs/<id>.json       the harness stamps
//	<cache>/bwrap/refs/<ref>          the id a ref was pulled as

// remoteImage is one image as its registry describes it for one arch.
type remoteImage struct {
	repo     *remote.Repository
	ref      string
	name     string // host/repo, as a docker repo digest names it
	digest   string // what the ref names: the index, or the manifest when there is none
	manifest string // the arch's manifest digest: the image's id here
	layers   []ocispec.Descriptor
	env      []string
}

// image resolves ref to its manifest for linux/arch and reads its config.
func (reg registry) image(ctx context.Context, ref, arch string) (*remoteImage, error) {
	nref := NormalizeRef(ref)
	repo, what, err := reg.repo(nref)
	if err != nil {
		return nil, err
	}
	top, man, id, err := platformManifest(ctx, repo, ref, what, arch)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"config"`
	}
	if err := fetchJSON(ctx, repo, man.Config, &cfg); err != nil {
		return nil, fmt.Errorf("%s: image config: %w", ref, err)
	}
	return &remoteImage{repo: repo, ref: ref, name: Repository(nref), digest: top.Digest.String(), manifest: id,
		layers: man.Layers, env: cfg.Config.Env}, nil
}

// fetchLayer downloads a layer whole into a temp file under dir and checks it
// against its digest before anything reads it.
func (ri *remoteImage) fetchLayer(ctx context.Context, l ocispec.Descriptor, dir string) (string, error) {
	return fetchBlob(ctx, ri.repo, l, dir, ri.ref)
}

// layerReader opens a fetched layer as a tar stream, by its media type.
func layerReader(file, mediaType string) (io.ReadCloser, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasSuffix(mediaType, "zstd"):
		zr, err := zstd.NewReader(f, zstd.WithDecoderMaxWindow(zstdMaxWindow))
		if err != nil {
			f.Close()
			return nil, err
		}
		return zstdLayer{zr, f}, nil
	case strings.HasSuffix(mediaType, "gzip"), strings.HasSuffix(mediaType, ".tar.gzip"), strings.HasSuffix(mediaType, "diff.tar.gzip"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{gz, f}, nil
	}
	return f, nil
}

// zstdMaxWindow is the most a zstd layer may ask its reader to keep in
// memory: the format's own ceiling for a stream made without asking for
// more, and what the zstd cli reads without being told to.
const zstdMaxWindow = 128 << 20

// zstdLayer is a zstd layer's tar stream and the file it is read from.
type zstdLayer struct {
	*zstd.Decoder
	file *os.File
}

func (z zstdLayer) Close() error {
	z.Decoder.Close()
	return z.file.Close()
}

// bwrapStore is the bwrap driver's image store.
type bwrapStore struct{ dir string }

func newBwrapStore() (bwrapStore, error) {
	c, err := CacheDir()
	if err != nil {
		return bwrapStore{}, err
	}
	return bwrapStore{dir: filepath.Join(c, "bwrap")}, nil
}

// storeKey is an image id as a file name. an id comes from a pin file as
// well as from a registry, so one that is no image id is never made a path
// of: it gets a name of its own, which no image is under.
func storeKey(id string) string {
	if !imageIDRe.MatchString(id) {
		sum := sha256.Sum256([]byte(id))
		return "no-image-" + hex.EncodeToString(sum[:8])
	}
	return strings.ReplaceAll(id, ":", "-")
}

func (st bwrapStore) rootfs(id string) string { return filepath.Join(st.dir, "rootfs", storeKey(id)) }
func (st bwrapStore) envFile(id string) string {
	return filepath.Join(st.dir, "rootfs", storeKey(id)+".env.json")
}
func (st bwrapStore) stampsFile(id string) string {
	return filepath.Join(st.dir, "rootfs", storeKey(id)+".json")
}
func (st bwrapStore) lockFile(id string) string {
	return filepath.Join(st.dir, "rootfs", storeKey(id)+".lock")
}
func (st bwrapStore) refFile(ref string) string {
	sum := sha256.Sum256([]byte(NormalizeRef(ref)))
	return filepath.Join(st.dir, "refs", hex.EncodeToString(sum[:]))
}

// harnessTree is where the tree of id is unpacked, and treeLock its lock.
func (st bwrapStore) harnessTree(id string) string { return filepath.Join(st.dir, "harness", id) }
func (st bwrapStore) treeLock(id string) string    { return filepath.Join(st.dir, "harness", id+".lock") }

// treeIDRe is a tree's id (Artefact.TreeID). an id comes from a pin file, and
// names a directory a session mounts: one that is anything else is no tree.
var treeIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hasTree says whether the tree of id is in the store, whole: it is renamed
// into place once unpacked.
func (st bwrapStore) hasTree(id string) bool {
	if !treeIDRe.MatchString(id) {
		return false
	}
	fi, err := os.Stat(st.harnessTree(id))
	return err == nil && fi.IsDir()
}

// purgeTrees removes every harness tree no running sandbox holds, and what a
// crashed unpack left of one; a tree in use is reported and kept. a tree goes
// in one step, renamed aside first: it is in the store whole or not at all.
func (st bwrapStore) purgeTrees(dryRun bool, report func(CleanEvent)) error {
	es, err := os.ReadDir(filepath.Join(st.dir, "harness"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var errs []error
	for _, e := range es {
		id := strings.TrimSuffix(e.Name(), ".new")
		if !e.IsDir() {
			continue // a lock file, which stays: an unpack may wait on it
		}
		tree := st.harnessTree(e.Name())
		if _, err := os.Lstat(tree); err != nil {
			continue // went with the tree it was beside
		}
		unlock, err := lockAt(context.Background(), st.treeLock(id), true, true)
		if err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				report(CleanEvent{Action: "keep", Kind: "tree", Name: tree, Why: "in use by a running sandbox, or being unpacked"})
				continue
			}
			errs = append(errs, err)
			continue
		}
		if !dryRun {
			// what a crashed unpack left there goes first, then the tree by
			// the same name.
			aside := st.harnessTree(id) + ".new"
			err = removeTree(aside)
			if err == nil && tree != aside {
				if err = os.Rename(tree, aside); err == nil {
					err = removeTree(aside)
				}
			}
		}
		unlock()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		report(CleanEvent{Action: "remove", Kind: "tree", Name: tree})
	}
	return errors.Join(errs...)
}

// lock opens id's lock file and takes it (exclusive or shared), waiting
// until ctx ends unless nowait; the returned func releases it.
func (st bwrapStore) lock(ctx context.Context, id string, exclusive, nowait bool) (func(), error) {
	return lockAt(ctx, st.lockFile(id), exclusive, nowait)
}

// lockAt is lock for the lock file at path.
func lockAt(ctx context.Context, path string, exclusive, nowait bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if nowait {
		how := syscall.LOCK_SH | syscall.LOCK_NB
		if exclusive {
			how = syscall.LOCK_EX | syscall.LOCK_NB
		}
		err = syscall.Flock(int(f.Fd()), how)
	} else {
		err = Flock(ctx, f, exclusive)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// has says whether id's rootfs is in the store, whole.
func (st bwrapStore) has(id string) bool {
	fi, err := os.Stat(st.rootfs(id))
	return err == nil && fi.IsDir()
}

// imageEnv is the image config's Env kept at unpack.
func (st bwrapStore) imageEnv(id string) ([]string, error) {
	b, err := os.ReadFile(st.envFile(id))
	if err != nil {
		return nil, err
	}
	var env []string
	return env, json.Unmarshal(b, &env)
}

// pull makes ri's rootfs present: unpacked into <id>.new under the image's
// lock and renamed when whole. one already there is kept unless force.
// fetched says whether this call unpacked it.
func (st bwrapStore) pull(ctx context.Context, ri *remoteImage, force bool, progress func(string)) (fetched bool, err error) {
	id := ri.manifest
	unlock, err := st.lock(ctx, id, true, false)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := os.MkdirAll(filepath.Join(st.dir, "refs"), 0o700); err != nil {
		return false, err
	}
	defer func() {
		if err == nil {
			err = writeAtomic(st.refFile(ri.ref), []byte(id+"\n"+ri.name+"@"+ri.digest+"\n"))
		}
	}()
	if st.has(id) && !force {
		return false, nil
	}
	tmp, dl := st.rootfs(id)+".new", st.rootfs(id)+".dl"
	if err := removeTree(tmp); err != nil {
		return false, err
	}
	// layers download beside it, under the same lock, so a crash's are
	// swept with its half-made rootfs.
	if err := os.MkdirAll(dl, 0o700); err != nil {
		return false, err
	}
	defer removeTree(dl)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return false, err
	}
	for i, l := range ri.layers {
		if progress != nil {
			progress(fmt.Sprintf("layer %d/%d %s", i+1, len(ri.layers), short12(l.Digest.String())))
		}
		file, err := ri.fetchLayer(ctx, l, dl)
		if err != nil {
			return false, err
		}
		err = unpackLayer(func() (io.ReadCloser, error) { return layerReader(file, l.MediaType) }, tmp)
		os.Remove(file)
		if err != nil {
			return false, fmt.Errorf("%s: layer %s: %w", ri.ref, l.Digest, err)
		}
	}
	env, _ := json.Marshal(ri.env)
	if err := os.WriteFile(st.envFile(id), env, 0o600); err != nil {
		return false, err
	}
	if err := removeTree(st.rootfs(id)); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, st.rootfs(id)); err != nil {
		return false, err
	}
	_ = os.Remove(st.stampsFile(id)) // stamps of a rootfs no longer there
	return true, nil
}

// resolveRef is the id and repo digest the store last pulled ref as.
func (st bwrapStore) resolveRef(ref string) (id, repoDigest string, ok bool) {
	b, err := os.ReadFile(st.refFile(ref))
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if !imageIDRe.MatchString(lines[0]) || !st.has(lines[0]) {
		return "", "", false
	}
	if len(lines) > 1 {
		repoDigest = lines[1]
	}
	return lines[0], repoDigest, true
}

// ids are the images the store holds whole.
func (st bwrapStore) ids() []string {
	es, _ := os.ReadDir(filepath.Join(st.dir, "rootfs"))
	var out []string
	for _, e := range es {
		if id := strings.Replace(e.Name(), "-", ":", 1); e.IsDir() && imageIDRe.MatchString(id) {
			out = append(out, id)
		}
	}
	return out
}

// remove takes id out of the store, refusing one a session holds.
func (st bwrapStore) remove(id string) error {
	unlock, err := st.lock(context.Background(), id, true, true)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("image %s is in use by a running sandbox", short12(id))
		}
		return err
	}
	defer unlock()
	for _, p := range []string{st.rootfs(id), st.rootfs(id) + ".new", st.rootfs(id) + ".dl"} {
		if err := removeTree(p); err != nil {
			return err
		}
	}
	// the lock file stays: another pull may already wait on it, and a new
	// inode would let both in.
	for _, p := range []string{st.envFile(id), st.stampsFile(id)} {
		_ = os.Remove(p)
	}
	return nil
}

// sweepNew removes what crashed unpacks left, their .new rootfs and .dl
// downloads: one whose lock is free, never an unpack in progress. a dry run
// says what it would remove.
func (st bwrapStore) sweepNew(dryRun bool) []string {
	es, _ := os.ReadDir(filepath.Join(st.dir, "rootfs"))
	var gone []string
	for _, e := range es {
		n, ok := strings.CutSuffix(e.Name(), ".new")
		if !ok {
			n, ok = strings.CutSuffix(e.Name(), ".dl")
		}
		if !ok || !e.IsDir() {
			continue
		}
		id := strings.Replace(n, "-", ":", 1)
		unlock, err := st.lock(context.Background(), id, true, true)
		if err != nil {
			continue
		}
		if dryRun || removeTree(filepath.Join(st.dir, "rootfs", e.Name())) == nil {
			gone = append(gone, e.Name())
		}
		unlock()
	}
	return gone
}

var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// writeAtomic puts b at p whole or not at all: a reader never sees it half
// written.
func writeAtomic(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func short12(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}
