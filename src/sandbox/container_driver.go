package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Target is docker:<context> with the daemon's id and version, or
// podman:<connection> with the connection's uri (podman has no daemon id),
// podman:local when it has no connection configured.
func (c *container) Target(ctx context.Context) (Target, error) {
	if c.driver == "podman" {
		// the cli's own overrides first: a service uri, then a named connection.
		if uri := c.o.ContainerHost; uri != "" {
			return Target{Key: "podman:" + uri, Daemon: uri}, nil
		}
		want := c.o.ContainerConnection
		out, err := c.dockerOut(ctx, "system", "connection", "list", "--format", "json")
		if err != nil {
			return Target{}, err
		}
		var conns []struct {
			Name    string
			URI     string
			Default bool
		}
		_ = json.Unmarshal(out, &conns)
		for _, cn := range conns {
			if (want != "" && cn.Name == want) || (want == "" && cn.Default) {
				return Target{Key: "podman:" + cn.Name, Daemon: cn.URI}, nil
			}
		}
		if want != "" {
			return Target{}, fmt.Errorf("podman connection %q (CONTAINER_CONNECTION) not found", want)
		}
		return Target{Key: "podman:local", Daemon: "local"}, nil
	}
	name, err := c.dockerOut(ctx, "context", "show")
	if err != nil {
		return Target{}, err
	}
	info, err := c.dockerOut(ctx, "info", "--format", "{{.ID}}|{{.ServerVersion}}")
	if err != nil {
		return Target{}, err
	}
	id, ver, _ := strings.Cut(strings.TrimSpace(string(info)), "|")
	return Target{Key: c.driver + ":" + strings.TrimSpace(string(name)), Daemon: id, Version: ver}, nil
}

// CPUs is the daemon's cpu count: on a desktop, its vm's.
func (c *container) CPUs(ctx context.Context) int {
	out, err := c.dockerOut(ctx, "info", "--format", engines[c.driver].cpus)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// Resolve is ref's id and, when it came from a registry, the digest of
// ref's repository: an image pulled from two repositories has one in each.
// an image id has the first.
func (c *container) Resolve(ctx context.Context, ref string) (Image, error) {
	out, err := c.dockerOut(ctx, "image", "inspect", "--format", "{{.Id}}|{{range .RepoDigests}}{{.}} {{end}}", ref)
	if err != nil {
		return Image{}, noImage(err)
	}
	id, digests, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	img := Image{ID: ImageID(id), Name: ref}
	want := Repository(ref)
	for i, rd := range strings.Fields(digests) {
		name, d, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		if Repository(name) == want || (i == 0 && isImageID(ref)) {
			img.Digest, img.Repo = d, name
			break
		}
	}
	return img, nil
}

// isImageID says whether ref names an image by id, whole or short.
func isImageID(ref string) bool {
	h := strings.TrimPrefix(ref, "sha256:")
	return len(h) >= 12 && isHex(h)
}

func (c *container) Images(ctx context.Context) (map[string]bool, error) {
	out, err := c.dockerOut(ctx, "images", "-a", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		ids[ImageID(id)] = true
	}
	return ids, nil
}

// ImageID spells a container image id one way, sha256:<hex>: podman's
// image inspect prints the hex alone, where its images and --iidfile, and
// docker everywhere, print the prefix.
func ImageID(id string) string {
	if isFingerprint(id) {
		return "sha256:" + id
	}
	return id
}

func (c *container) Pull(ctx context.Context, ref string, force bool, log io.Writer) (Image, bool, error) {
	if !force {
		if img, err := c.Resolve(ctx, ref); err == nil {
			return img, false, nil
		} else if !errors.Is(err, ErrNoImage) {
			return Image{}, false, err
		}
	}
	if log == nil {
		log = io.Discard
	}
	var errb bytes.Buffer
	if err := c.r.Run(ctx, nil, log, io.MultiWriter(log, &errb), "pull", ref); err != nil {
		return Image{}, false, fmt.Errorf("%s pull %s: %w%s", c.driver, ref, err, stderrTail(&errb))
	}
	img, err := c.Resolve(ctx, ref)
	return img, err == nil, err
}

// Build is a build: dockerfile, or one generated for base / provision:
// the provision step run on spec.Base (the pats sandbox when empty), with
// the material as the context. the base returned is the pats sandbox, when
// the build used it.
func (c *container) Build(ctx context.Context, spec BuildSpec, log io.Writer) (Image, string, bool, error) {
	sandbox := spec.SandboxImage
	if sandbox == nil {
		sandbox = func() string { img, _ := SandboxImage(""); return img }
	}
	b := containerBuild{Dir: spec.Dir, Dockerfile: spec.Dockerfile, Force: spec.Force, Log: log, Progress: spec.Progress}
	if spec.Dockerfile == "" {
		gen, err := MkTemp("pats-build-")
		if err != nil {
			return Image{}, "", false, err
		}
		defer RemoveTemp(gen)
		b.Dockerfile = filepath.Join(gen, "Dockerfile")
		if err := os.WriteFile(b.Dockerfile, []byte(provisionDockerfile(spec, sandbox)), 0o644); err != nil {
			return Image{}, "", false, err
		}
		b.Dir = spec.Material
		if b.Dir == "" {
			// no material: the context is empty, and nothing reads it.
			b.Dir = filepath.Join(gen, "context")
			if err := os.Mkdir(b.Dir, 0o755); err != nil {
				return Image{}, "", false, err
			}
		}
	}
	id, base, built, err := buildContainerImage(ctx, c.r, c.driver, b, sandbox)
	if err != nil {
		return Image{}, "", false, err
	}
	return Image{ID: ImageID(id)}, base, built, nil
}

// provisionDockerfile runs spec's provision step on its base, with its
// material copied in for the step and removed after. with no base it builds
// on the pats sandbox, which the build arg names and the default repeats so
// the dockerfile reads as valid on its own.
func provisionDockerfile(spec BuildSpec, sandbox func() string) string {
	arg, from := baseArg, spec.Base
	if from == "" {
		arg, from = baseArg+"="+sandbox(), "${"+baseArg+"}"
	}
	var df strings.Builder
	fmt.Fprintf(&df, "ARG %s\nFROM %s\nUSER root\n", arg, from)
	if len(spec.Argv) > 0 {
		if spec.Material != "" {
			fmt.Fprintf(&df, "COPY . %s\n", ProvisionMount)
		}
		run, _ := json.Marshal(provisionArgv(spec.Argv))
		fmt.Fprintf(&df, "RUN %s\n", run)
		fmt.Fprintf(&df, "RUN rm -rf %s\n", ProvisionMount)
	}
	return df.String()
}

// provisionArgv runs argv with the material's bin/ first on PATH.
func provisionArgv(argv []string) []string {
	return append([]string{"/bin/sh", "-c", `PATH="` + ProvisionMount + `/bin:$PATH"; export PATH; exec "$@"`, "provision"}, argv...)
}

// ProxyImage pulls the published proxy image, or the one ref names.
func (c *container) ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (Image, bool, error) {
	return c.Pull(ctx, ref, force, log)
}

func (c *container) Stamps(ctx context.Context, id string) (map[string]string, error) {
	return c.stamps(ctx, id, stampTimeout)
}

func (c *container) RemoveImage(ctx context.Context, id string) error {
	_, err := c.dockerOut(ctx, "rmi", id) // no -f: an image a container uses stays
	if err != nil && notFound(err) {
		return nil
	}
	return err
}

// Upstream is the digest ref's tag names in its registry now.
func (c *container) Upstream(ctx context.Context, ref string) (string, error) {
	return c.p.reg.tagDigest(ctx, ref)
}
