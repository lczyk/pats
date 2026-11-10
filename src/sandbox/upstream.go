package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
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
	repo, what, err := reg.repo(NormalizeRef(ref))
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, what)
	if err != nil {
		return "", fmt.Errorf("registry manifest %s: %w", ref, err)
	}
	return desc.Digest.String(), nil
}

// debianArch is the suffix image remotes put on an alias for each kernel
// architecture (26.04/arm64).
var debianArch = map[string]string{
	"x86_64": "amd64", "aarch64": "arm64", "armv7l": "armhf", "ppc64le": "ppc64el",
	"s390x": "s390x", "riscv64": "riscv64",
}

// remoteFingerprint is the fingerprint an image remote's alias names now
// (ubuntu:26.04) for l's server: its architecture, and a container or a vm
// as l says. read with image list, which reaches simplestreams remotes as
// well as lxd ones.
func (l *lxd) remoteFingerprint(ctx context.Context, ref string) (string, error) {
	remote, alias, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("lxd image %q names no remote", ref)
	}
	arch, err := l.kernelArch(ctx)
	if err != nil {
		return "", err
	}
	kind := "container"
	if l.o.VM {
		kind = "virtual-machine"
	}
	out, err := l.lxc(ctx, "image", "list", remote+":", alias, "--format=json")
	if err != nil {
		return "", err
	}
	var ims []struct {
		Fingerprint  string `json:"fingerprint"`
		Architecture string `json:"architecture"`
		Type         string `json:"type"`
		Aliases      []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(out), &ims); err != nil {
		return "", err
	}
	// only what the server can run as asked: a short alias belongs to one
	// architecture, the others carry it with their own suffix.
	fit := ims[:0]
	for _, im := range ims {
		if (im.Architecture == "" || im.Architecture == arch) && (im.Type == "" || im.Type == kind) {
			fit = append(fit, im)
		}
	}
	// the filter matches on prefixes too: the alias itself decides, or a
	// fingerprint (or a unique prefix of one) the ref names.
	names := map[string]bool{alias: true, alias + "/" + debianArch[arch]: true}
	for _, im := range fit {
		for _, a := range im.Aliases {
			if names[a.Name] {
				return im.Fingerprint, nil
			}
		}
	}
	if isHex(alias) {
		var hits []string
		for _, im := range fit {
			if strings.HasPrefix(im.Fingerprint, alias) {
				hits = append(hits, im.Fingerprint)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
	}
	return "", fmt.Errorf("%w: lxd image %s is not listed for %s %s", ErrNoImage, ref, arch, kind)
}

// kernelArch is the server's architecture as lxd names it (x86_64).
func (l *lxd) kernelArch(ctx context.Context) (string, error) {
	if l.o.Remote == "" {
		remote, err := l.defaultRemote(ctx)
		if err != nil {
			return "", err
		}
		c := *l
		c.o.Remote = remote
		l = &c
	}
	var srv struct {
		Environment struct {
			Architectures []string `json:"architectures"`
		} `json:"environment"`
	}
	if err := l.query(ctx, "/1.0", &srv); err != nil {
		return "", err
	}
	if len(srv.Environment.Architectures) == 0 {
		return "", fmt.Errorf("lxd: server %s names no architecture", l.o.Remote)
	}
	return srv.Environment.Architectures[0], nil
}
