package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
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
	c        *registryClient
	ref      string
	repo     string // host/repo, as a docker repo digest names it
	digest   string // what the ref names: the index, or the manifest when there is none
	manifest string // the arch's manifest digest: the image's id here
	layers   []layerDesc
	env      []string
}

type layerDesc struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

// splitRefDigest is splitRef that also takes host/repo@sha256:...: the
// "tag" is then the digest.
func splitRefDigest(ref string) (host, repo, tag string, err error) {
	if name, d, ok := strings.Cut(ref, "@"); ok {
		host, repo, _, err = splitRef(name)
		return host, repo, d, err
	}
	return splitRef(ref)
}

// image resolves ref to its manifest for linux/arch and reads its config.
func (reg registry) image(ctx context.Context, ref, arch string) (*remoteImage, error) {
	nref := NormalizeRef(ref)
	host, repo, tag, err := splitRefDigest(nref)
	if err != nil {
		return nil, err
	}
	api := host
	if host == "docker.io" {
		api = "registry-1.docker.io"
	}
	c := &registryClient{ctx: ctx, http: reg.client, root: reg.url(api), base: reg.url(api) + "/v2/" + repo}
	if err := c.challenge(); err != nil {
		return nil, err
	}
	digest, mediaType, err := c.manifest(tag)
	if err != nil {
		return nil, err
	}
	// a ref pinned by digest is that digest: the registry may not name
	// another, and the manifests below are checked against it.
	if strings.HasPrefix(tag, "sha256:") {
		if digest != "" && digest != tag {
			return nil, fmt.Errorf("%s: the registry answered with %s", ref, digest)
		}
		digest = tag
	}
	ri := &remoteImage{c: c, ref: ref, repo: host + "/" + repo, digest: digest, manifest: digest}
	if strings.Contains(mediaType, "index") || strings.Contains(mediaType, "manifest.list") {
		var idx struct {
			Manifests []struct {
				Digest   string `json:"digest"`
				Platform struct {
					OS           string `json:"os"`
					Architecture string `json:"architecture"`
				} `json:"platform"`
			} `json:"manifests"`
		}
		if err := c.getVerified("/manifests/"+digest, indexAccept, digest, &idx); err != nil {
			return nil, err
		}
		ri.manifest = ""
		for _, m := range idx.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				ri.manifest = m.Digest
				break
			}
		}
		if ri.manifest == "" {
			return nil, fmt.Errorf("%s has no linux/%s image", ref, arch)
		}
	}
	var man struct {
		Config layerDesc   `json:"config"`
		Layers []layerDesc `json:"layers"`
	}
	if err := c.getVerified("/manifests/"+ri.manifest, manifestAccept, ri.manifest, &man); err != nil {
		return nil, err
	}
	if len(man.Layers) == 0 {
		return nil, fmt.Errorf("%s: no layers", ref)
	}
	ri.layers = man.Layers
	var cfg struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"config"`
	}
	if err := c.getVerified("/blobs/"+man.Config.Digest, "", man.Config.Digest, &cfg); err != nil {
		return nil, fmt.Errorf("%s: image config: %w", ref, err)
	}
	ri.env = cfg.Config.Env
	return ri, nil
}

// challenge gets an anonymous token the way the registry asks for one (ghcr,
// docker hub), or none when it asks for none.
func (c *registryClient) challenge() error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.root+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return nil
	}
	// the bare /v2/ challenge names no scope, or a placeholder one (ghcr's
	// "user/image"): the scope is always this repo's pull.
	ch := scopeRe.ReplaceAllString(resp.Header.Get("WWW-Authenticate"), "")
	repo := strings.TrimPrefix(c.base, c.root+"/v2/")
	ch = strings.TrimRight(ch, ", ") + `,scope="repository:` + repo + `:pull"`
	c.token, err = registry{client: c.http}.challengeToken(c.ctx, ch)
	return err
}

var scopeRe = regexp.MustCompile(`,?\s*scope="[^"]*"`)

// fetchLayer downloads a layer whole into a temp file under dir and checks it
// against its digest before anything reads it.
func (ri *remoteImage) fetchLayer(l layerDesc, dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".pats-layer-")
	if err != nil {
		return "", err
	}
	body, err := ri.c.get("/blobs/"+l.Digest, "")
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	body.Close()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != l.Digest {
			err = fmt.Errorf("%s: layer digest %s, want %s", ri.ref, got, l.Digest)
		}
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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

// applyWhiteout removes what a whiteout in dir names: the opaque marker
// empties dir, any other names one entry. a dir that is not there has
// nothing to remove.
func applyWhiteout(root *os.Root, dir, base string) error {
	name := strings.TrimPrefix(base, ".wh.")
	if base != ".wh..wh..opq" {
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("a whiteout of %q %w", name, errEscape)
		}
		return root.RemoveAll(path.Join(dir, name))
	}
	d, err := root.Open(path.Join(dir, "."))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if errors.Is(err, syscall.ENOTDIR) {
		return nil // a file a lower layer left there has nothing in it
	}
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := root.RemoveAll(path.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

// unpackLayer applies one layer's tar to the rootfs in dir: whiteouts and
// opaque dirs remove what lower layers put there, device nodes are skipped
// (bwrap supplies /dev), setuid and setgid bits are dropped, ownership is not
// kept, and an entry whose path or hard link leaves the rootfs is refused.
// every entry is written through dir's root, so one that reaches past a
// symlink lands where that leads inside the rootfs, or is refused.
//
// whiteouts go first, in a pass of their own: they remove what lower layers
// put there, never what this layer adds, whatever the order in its tar.
func unpackLayer(open func() (io.ReadCloser, error), dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, whiteouts := range []bool{true, false} {
		r, err := open()
		if err != nil {
			return err
		}
		err = applyLayer(r, root, whiteouts)
		r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// applyLayer is one pass of unpackLayer: the whiteouts alone, or the rest.
func applyLayer(r io.Reader, root *os.Root, whiteouts bool) error {
	tr := tar.NewReader(r)
	// the directories this pass knows are there, so each is looked for once:
	// a root walks a path anew on every call. forgotten when an entry takes
	// the place of another, which may have been one of them.
	made := map[string]bool{}
	replace := func(rel string) error {
		clear(made)
		return root.RemoveAll(rel)
	}
	// the whiteouts pass: the whiteouts, and the directories the layer
	// brings, which say where a whiteout has nothing below to remove.
	var whs [][3]string // an entry's name, its dir and its base
	dirs := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			for _, wh := range whs {
				if ownDir(root, dirs, wh[1]) {
					continue
				}
				if err := applyWhiteout(root, wh[1], wh[2]); err != nil {
					return fmt.Errorf("%s: %w", wh[0], err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := entryPath(h.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the root itself
		}
		dir, base := path.Split(rel)
		wh := strings.HasPrefix(base, ".wh.")
		if whiteouts {
			switch {
			case wh:
				whs = append(whs, [3]string{h.Name, dir, base})
			case h.Typeflag == tar.TypeDir:
				dirs[rel] = true
			}
			continue
		}
		if wh {
			continue
		}
		if parent := path.Dir(rel); !made[parent] {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			made[parent] = true
		}
		mode := fs.FileMode(h.Mode).Perm() // setuid and setgid dropped
		if h.Mode&01000 != 0 {
			mode |= fs.ModeSticky
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err := root.Mkdir(rel, 0o700)
			if errors.Is(err, fs.ErrExist) {
				// a directory already there is kept; anything else, a link
				// to one too, gives way.
				if fi, lerr := root.Lstat(rel); lerr == nil && fi.IsDir() {
					err = nil
				} else if err = replace(rel); err == nil {
					err = root.Mkdir(rel, 0o700)
				}
			}
			if err != nil {
				return err
			}
			// the owner (pats's user) must be able to fill it.
			if err := root.Chmod(rel, mode|0o700); err != nil {
				return err
			}
			made[rel] = true
		case tar.TypeReg:
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm()|0o600)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					f, err = root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm()|0o600)
				}
			}
			if err != nil {
				return err
			}
			_, err = copyFileBytes(f, tr)
			if err == nil && mode&fs.ModeSticky != 0 {
				// a root's OpenFile takes the nine bits only: the tenth is
				// added to the ones the umask left.
				var fi fs.FileInfo
				if fi, err = f.Stat(); err == nil {
					err = f.Chmod(fi.Mode().Perm() | fs.ModeSticky)
				}
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			// how far a link may climb is counted from where its path
			// says it is, which a link on that path would move.
			if linkEscapes(dir, h.Linkname) || (strings.Contains(h.Linkname, "..") && throughLink(root, dir)) {
				return fmt.Errorf("%s: symlink to %s %w", h.Name, h.Linkname, errEscape)
			}
			err := root.Symlink(h.Linkname, rel)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					err = root.Symlink(h.Linkname, rel)
				}
			}
			if err != nil {
				return err
			}
		case tar.TypeLink:
			src, err := entryPath(h.Linkname)
			if err != nil || src == "" {
				return fmt.Errorf("%s: hard link to %s %w", h.Name, h.Linkname, errEscape)
			}
			if src == rel {
				break // a name linked to itself is the entry it already is
			}
			err = root.Link(src, rel)
			if errors.Is(err, fs.ErrExist) {
				if err = replace(rel); err == nil {
					err = root.Link(src, rel)
				}
			}
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
		default:
			// devices, fifos and the like: bwrap supplies /dev.
		}
	}
}

// entryPath is a tar entry's path relative to the rootfs, "" for the root;
// one with a .. component is refused, never clamped.
func entryPath(name string) (string, error) {
	if len(name) > maxPath {
		return "", fmt.Errorf("%.40s...: %w", name, errLong)
	}
	n := strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/")
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("%s: %w", name, errEscape)
		}
	}
	n = path.Clean("/" + n)
	return strings.TrimPrefix(n, "/"), nil
}

// ownDir says whether dir lies in a directory the layer brings (dirs) where
// the rootfs holds something else, a link to a directory too. the layer's
// own directory takes that place, with nothing of the lower layers in it:
// a whiteout there has nothing to remove, and followed through the link
// would remove what the link points at.
func ownDir(root *os.Root, dirs map[string]bool, dir string) bool {
	p := ""
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		p = path.Join(p, part)
		if !dirs[p] {
			continue
		}
		if fi, err := root.Lstat(p); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// throughLink says whether a component of dir is a symlink.
func throughLink(root *os.Root, dir string) bool {
	p := ""
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		p = path.Join(p, part)
		if fi, err := root.Lstat(p); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// linkEscapes says whether a symlink in dir pointing at target climbs out of
// the rootfs. an absolute target resolves inside the rootfs at run time.
func linkEscapes(dir, target string) bool {
	if path.IsAbs(target) {
		return false
	}
	depth := 0
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part != "" {
			depth++
		}
	}
	for _, part := range strings.Split(target, "/") {
		switch part {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
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

// lock opens id's lock file and takes it (exclusive or shared), waiting
// unless nowait; the returned func releases it.
func (st bwrapStore) lock(id string, exclusive, nowait bool) (func(), error) {
	if err := os.MkdirAll(filepath.Join(st.dir, "rootfs"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(st.lockFile(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if nowait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
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
func (st bwrapStore) pull(ri *remoteImage, force bool, progress func(string)) (fetched bool, err error) {
	id := ri.manifest
	unlock, err := st.lock(id, true, false)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := os.MkdirAll(filepath.Join(st.dir, "refs"), 0o700); err != nil {
		return false, err
	}
	defer func() {
		if err == nil {
			err = writeAtomic(st.refFile(ri.ref), []byte(id+"\n"+ri.repo+"@"+ri.digest+"\n"))
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
			progress(fmt.Sprintf("layer %d/%d %s", i+1, len(ri.layers), short12(l.Digest)))
		}
		file, err := ri.fetchLayer(l, dl)
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
	unlock, err := st.lock(id, true, true)
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
		unlock, err := st.lock(id, true, true)
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

// removeTree removes p, making what is under it writable first: an unpacked
// image has read-only dirs, and overlayfs leaves its work dir mode 000.
func removeTree(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// a dir is visited before it is read, so its chmod comes first.
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

func short12(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}
