package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// NormalizeRef spells an image ref the way a registry is asked for it:
// docker hub's short forms gain their host and library/ (ubuntu:26.04 ->
// docker.io/library/ubuntu:26.04), a ref with no tag gains :latest.
func NormalizeRef(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	if !ok || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		if !ok {
			ref = "library/" + ref
		}
		ref = "docker.io/" + ref
	}
	// docker hub answers to several names; docker records them as docker.io.
	for _, alias := range []string{"index.docker.io/", "registry-1.docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, alias); ok {
			ref = "docker.io/" + rest
		}
	}
	// docker hub's official images live under library/, however named.
	if rest, ok := strings.CutPrefix(ref, "docker.io/"); ok && !strings.Contains(rest, "/") {
		ref = "docker.io/library/" + rest
	}
	if i := strings.LastIndexByte(ref, ':'); i < strings.LastIndexByte(ref, '/') {
		ref += ":latest"
	}
	return ref
}

// Repository is ref's normalised repository, without its tag or digest:
// what a RepoDigests entry names.
func Repository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	n := NormalizeRef(ref)
	if i := strings.LastIndexByte(n, ':'); i > strings.LastIndexByte(n, '/') {
		n = n[:i]
	}
	return n
}

// RegistryDigest is the digest a tag names in its registry now, by a HEAD of
// its manifest, with an anonymous token when the registry asks for one.
func RegistryDigest(ctx context.Context, ref string) (string, error) {
	return defaultRegistry.tagDigest(ctx, ref)
}

func (reg registry) tagDigest(ctx context.Context, ref string) (string, error) {
	host, repo, tag, err := splitRef(NormalizeRef(ref))
	if err != nil {
		return "", err
	}
	if host == "docker.io" {
		host = "registry-1.docker.io" // docker hub's api host
	}
	u := reg.url(host) + "/v2/" + repo + "/manifests/" + tag
	head := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", indexAccept+", "+manifestAccept)
		return reg.client.Do(req)
	}
	resp, err := head("")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		token, err := reg.challengeToken(ctx, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return "", err
		}
		if resp, err = head(token); err != nil {
			return "", err
		}
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry manifest %s: %s", ref, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("registry manifest %s: no digest", ref)
	}
	return d, nil
}

// challengeToken follows a Bearer challenge (realm, service, scope) to an
// anonymous token, as docker hub and ghcr both issue them.
func (reg registry) challengeToken(ctx context.Context, challenge string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", fmt.Errorf("registry: unsupported auth challenge %q", challenge)
	}
	kv := map[string]string{}
	for _, p := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		kv[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	if kv["realm"] == "" {
		return "", fmt.Errorf("registry: auth challenge with no realm")
	}
	q := url.Values{}
	for _, k := range []string{"service", "scope"} {
		if kv[k] != "" {
			q.Set(k, kv[k])
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, kv["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := reg.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token: %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

// LXDRemoteFingerprint is the fingerprint an image remote's alias names now
// (ubuntu:26.04), read with `lxc image list`, which reaches simplestreams
// remotes as well as lxd ones.
func LXDRemoteFingerprint(ctx context.Context, ref string) (string, error) {
	return newLXD(defaultProcess, lxcCLI, Options{}).remoteFingerprint(ctx, ref)
}

func (l *lxd) remoteFingerprint(ctx context.Context, ref string) (string, error) {
	remote, alias, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("lxd image %q names no remote", ref)
	}
	out, err := l.lxc(ctx, "image", "list", remote+":", alias, "--format=json")
	if err != nil {
		return "", err
	}
	var ims []struct {
		Fingerprint string `json:"fingerprint"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(out), &ims); err != nil {
		return "", err
	}
	// the filter matches on prefixes too: the alias itself decides, or a
	// fingerprint (or a unique prefix of one) the ref names.
	for _, im := range ims {
		for _, a := range im.Aliases {
			if a.Name == alias {
				return im.Fingerprint, nil
			}
		}
	}
	if isHex(alias) {
		var hits []string
		for _, im := range ims {
			if strings.HasPrefix(im.Fingerprint, alias) {
				hits = append(hits, im.Fingerprint)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
	}
	return "", fmt.Errorf("lxd image %s: not listed", ref)
}
