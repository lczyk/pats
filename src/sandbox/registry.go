package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// registry is how a process reaches image registries: the http client, and
// the base url of a registry host. production uses https and the default
// client; a test points both at an httptest server.
type registry struct {
	client *http.Client
	url    func(host string) string
}

var defaultRegistry = registry{client: http.DefaultClient, url: func(host string) string { return "https://" + host }}

// fetchImageFile copies one file out of a public image's top layer, straight
// from the registry's http api (token, index, per-arch manifest, blob): the
// lxd driver gets the proxy binary for the server's arch this way, with no
// docker on the machine pats runs on. ref is host/repo:tag; arch is a go arch.
func (reg registry) fetchImageFile(ctx context.Context, ref, arch, name, dst string) error {
	host, repo, tag, err := splitRef(ref)
	if err != nil {
		return err
	}
	c := &registryClient{ctx: ctx, http: reg.client, root: reg.url(host), base: reg.url(host) + "/v2/" + repo}
	if err := c.auth(host, repo); err != nil {
		return err
	}
	digest, mediaType, err := c.manifest(tag)
	if err != nil {
		return err
	}
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
			return err
		}
		digest = ""
		for _, m := range idx.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				digest = m.Digest
			}
		}
		if digest == "" {
			return fmt.Errorf("%s has no linux/%s image", ref, arch)
		}
	}
	var man struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := c.getVerified("/manifests/"+digest, manifestAccept, digest, &man); err != nil {
		return err
	}
	if len(man.Layers) == 0 {
		return fmt.Errorf("%s: no layers", ref)
	}
	// the layer is downloaded whole and checked against its digest before
	// anything in it is trusted; the binary goes in place only after.
	layer := man.Layers[len(man.Layers)-1].Digest
	blob, err := os.CreateTemp(filepath.Dir(dst), ".pats-layer-")
	if err != nil {
		return err
	}
	defer os.Remove(blob.Name())
	defer blob.Close()
	body, err := c.get("/blobs/"+layer, "")
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(blob, h), body)
	body.Close()
	if err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != layer {
		return fmt.Errorf("%s: layer digest %s, want %s", ref, got, layer)
	}
	if _, err := blob.Seek(0, io.SeekStart); err != nil {
		return err
	}
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

const (
	indexAccept    = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"
	manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
)

// splitRef splits host/repo:tag; a ref without a tag is :latest.
func splitRef(ref string) (host, repo, tag string, err error) {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || !strings.Contains(host, ".") {
		return "", "", "", fmt.Errorf("image ref %q names no registry host", ref)
	}
	repo, tag = rest, "latest"
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		repo, tag = rest[:i], rest[i+1:]
	}
	return host, repo, tag, nil
}

type registryClient struct {
	ctx   context.Context
	http  *http.Client
	root  string // the registry host's base url
	base  string // the repo's, under /v2
	token string
}

// auth gets an anonymous pull token, the way a public ghcr image is read.
func (c *registryClient) auth(host, repo string) error {
	u := c.root + "/token?service=" + url.QueryEscape(host) + "&scope=" + url.QueryEscape("repository:"+repo+":pull")
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", resp.Status)
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	c.token = t.Token
	return nil
}

func (c *registryClient) get(path, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return resp.Body, nil
}

// getVerified fetches a manifest by digest and checks its bytes hash to it
// before decoding.
func (c *registryClient) getVerified(path, accept, digest string, v any) error {
	body, err := c.get(path, accept)
	if err != nil {
		return err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("registry %s: digest %s, want %s", path, got, digest)
	}
	return json.Unmarshal(b, v)
}

// manifest resolves tag to a digest and says whether it is an index.
func (c *registryClient) manifest(tag string) (digest, mediaType string, err error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodHead, c.base+"/manifests/"+tag, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", indexAccept+", "+manifestAccept)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("registry manifest %s: %s", tag, resp.Status)
	}
	return resp.Header.Get("Docker-Content-Digest"), resp.Header.Get("Content-Type"), nil
}
