package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasreg "oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// registry is how a process reaches image registries: the http client, and
// the base url of a registry host. production uses https and the default
// client; a test points both at an httptest server. the protocol itself --
// the token dance, manifests by tag and digest, blobs checked against their
// digest and size -- is oras-go's.
type registry struct {
	client *http.Client
	url    func(host string) string
}

var defaultRegistry = registry{client: http.DefaultClient, url: func(host string) string { return "https://" + host }}

// maxManifest bounds a manifest, an index or an image config read whole.
const maxManifest = 4 << 20

// repo is ref's repository and what of it ref names, a tag or a digest. ref
// is spelt out: host/repo[:tag|@digest]. pulls are anonymous.
func (reg registry) repo(ref string) (*remote.Repository, string, error) {
	r, err := orasreg.ParseReference(ref)
	if err != nil {
		return nil, "", fmt.Errorf("image ref %q: %w", ref, err)
	}
	if !strings.ContainsAny(r.Registry, ".:") && r.Registry != "localhost" {
		return nil, "", fmt.Errorf("image ref %q names no registry host", ref)
	}
	what := r.Reference
	if what == "" {
		what = "latest"
	}
	// where the host is reached: its api host over https, or a test's server.
	base, err := url.Parse(reg.url(r.Host()))
	if err != nil {
		return nil, "", err
	}
	repo := &remote.Repository{
		Client:    &auth.Client{Client: reg.client, Cache: auth.NewCache()},
		Reference: orasreg.Reference{Registry: base.Host, Repository: r.Repository},
		PlainHTTP: base.Scheme == "http",
	}
	return repo, what, nil
}

// fetchJSON reads what desc names, checked against its digest and size, into v.
func fetchJSON(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, v any) error {
	if desc.Size > maxManifest {
		return fmt.Errorf("%s is %d bytes, more than a manifest may be", desc.Digest, desc.Size)
	}
	b, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// platformManifest resolves what to the manifest of its linux/arch image:
// itself, or the one an index lists. top is what the name resolved to.
func platformManifest(ctx context.Context, repo *remote.Repository, ref, what, arch string) (top ocispec.Descriptor, man ocispec.Manifest, id string, err error) {
	top, err = repo.Resolve(ctx, what)
	if err != nil {
		return top, man, "", fmt.Errorf("registry manifest %s: %w", ref, err)
	}
	desc := top
	if strings.Contains(top.MediaType, "index") || strings.Contains(top.MediaType, "manifest.list") {
		var idx ocispec.Index
		if err := fetchJSON(ctx, repo, top, &idx); err != nil {
			return top, man, "", err
		}
		found := false
		for _, m := range idx.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				desc, found = m, true
				break
			}
		}
		if !found {
			return top, man, "", fmt.Errorf("%s has no linux/%s image", ref, arch)
		}
	}
	if err := fetchJSON(ctx, repo, desc, &man); err != nil {
		return top, man, "", err
	}
	if len(man.Layers) == 0 {
		return top, man, "", fmt.Errorf("%s: no layers", ref)
	}
	return top, man, desc.Digest.String(), nil
}

// fetchBlob downloads a blob whole into a temp file under dir and checks it
// against its digest and size before anything reads it.
func fetchBlob(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, dir, ref string) (string, error) {
	f, err := os.CreateTemp(dir, ".pats-layer-")
	if err != nil {
		return "", err
	}
	err = func() error {
		rc, err := repo.Blobs().Fetch(ctx, desc)
		if err != nil {
			return err
		}
		defer rc.Close()
		vr := content.NewVerifyReader(rc, desc)
		if _, err := copyFileBytes(f, vr); err != nil {
			return err
		}
		return vr.Verify()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("%s: layer %s: %w", ref, desc.Digest, err)
	}
	return f.Name(), nil
}

// fetchImageFile copies one file out of a public image's top layer, straight
// from the registry: the lxd driver gets the proxy binary for the server's
// arch this way, with no docker on the machine pats runs on. ref is
// host/repo:tag; arch is a go arch.
func (reg registry) fetchImageFile(ctx context.Context, ref, arch, name, dst string) error {
	repo, what, err := reg.repo(ref)
	if err != nil {
		return err
	}
	_, man, _, err := platformManifest(ctx, repo, ref, what, arch)
	if err != nil {
		return err
	}
	// the layer is downloaded whole and checked before anything in it is
	// trusted; the binary goes in place only after.
	layer, err := fetchBlob(ctx, repo, man.Layers[len(man.Layers)-1], filepath.Dir(dst), ref)
	if err != nil {
		return err
	}
	defer os.Remove(layer)
	blob, err := os.Open(layer)
	if err != nil {
		return err
	}
	defer blob.Close()
	gz, err := gzip.NewReader(blob)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s: no %s in its top layer", ref, name)
		}
		if err != nil {
			return err
		}
		if strings.TrimPrefix(h.Name, "./") != strings.TrimPrefix(name, "/") || h.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.CreateTemp(filepath.Dir(dst), ".pats-bin-")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.Name(), 0o755)
		}
		if err != nil {
			return err
		}
		return os.Rename(f.Name(), dst)
	}
}
