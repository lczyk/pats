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
	"strings"
)

// fetchImageFile copies one file out of a public image's top layer, straight
// from the registry's http api (token, index, per-arch manifest, blob): the
// lxd driver gets the proxy binary for the server's arch this way, with no
// docker on the machine pats runs on. ref is host/repo:tag; arch is a go arch.
func fetchImageFile(ctx context.Context, ref, arch, name, dst string) error {
	host, repo, tag, err := splitRef(ref)
	if err != nil {
		return err
	}
	c := &registryClient{ctx: ctx, base: "https://" + host + "/v2/" + repo}
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
		if err := c.getJSON("/manifests/"+digest, indexAccept, &idx); err != nil {
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
	if err := c.getJSON("/manifests/"+digest, manifestAccept, &man); err != nil {
		return err
	}
	if len(man.Layers) == 0 {
		return fmt.Errorf("%s: no layers", ref)
	}
	body, err := c.get("/blobs/"+man.Layers[len(man.Layers)-1].Digest, "")
	if err != nil {
		return err
	}
	defer body.Close()
	gz, err := gzip.NewReader(body)
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
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
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
	base  string
	token string
}

// auth gets an anonymous pull token, the way a public ghcr image is read.
func (c *registryClient) auth(host, repo string) error {
	u := "https://" + host + "/token?service=" + url.QueryEscape(host) + "&scope=" + url.QueryEscape("repository:"+repo+":pull")
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return resp.Body, nil
}

func (c *registryClient) getJSON(path, accept string, v any) error {
	body, err := c.get(path, accept)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

// manifest resolves tag to a digest and says whether it is an index.
func (c *registryClient) manifest(tag string) (digest, mediaType string, err error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodHead, c.base+"/manifests/"+tag, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", indexAccept+", "+manifestAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("registry manifest %s: %s", tag, resp.Status)
	}
	return resp.Header.Get("Docker-Content-Digest"), resp.Header.Get("Content-Type"), nil
}
