package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// ready is a copy of l with its server prepared: a driver is shared by
// callers that may run at once, and preparing pins fields on the copy.
func (l *lxd) ready(ctx context.Context) (*lxd, error) {
	c := *l
	return &c, c.prepareServer(ctx)
}

// Sandbox is l for sandboxes of o.Image.
func (l *lxd) Sandbox(o SandboxOptions) (Sandbox, error) {
	if o.Image == "" {
		return nil, errors.New("sandbox driver lxd needs an image")
	}
	return l.on(o), nil
}

// on is a copy of l for sandboxes of o.
func (l *lxd) on(o SandboxOptions) *lxd {
	c := *l
	c.s = o
	return &c
}

// Target is lxd:<remote>, the remote resolved now, with the server's
// version.
func (l *lxd) Target(ctx context.Context) (Target, error) {
	remote := l.o.Remote
	if remote == "" {
		var err error
		if remote, err = l.defaultRemote(ctx); err != nil {
			return Target{}, err
		}
	}
	out, err := l.lxc(ctx, "query", remote+":/1.0")
	if err != nil {
		return Target{}, err
	}
	var srv struct {
		Environment struct {
			ServerVersion string `json:"server_version"`
		} `json:"environment"`
	}
	if err := json.Unmarshal([]byte(out), &srv); err != nil {
		return Target{}, err
	}
	return Target{Key: "lxd:" + remote, Version: srv.Environment.ServerVersion}, nil
}

// CPUs is the server's cpu count: -j auto sizes the run by the machine the
// pairs run on.
func (l *lxd) CPUs(ctx context.Context) int {
	p := "/1.0/resources"
	if l.o.Remote != "" {
		p = l.o.Remote + ":" + p
	}
	out, err := l.lxc(ctx, "query", p)
	if err != nil {
		return 0
	}
	var res struct {
		CPU struct {
			Total int `json:"total"`
		} `json:"cpu"`
	}
	if json.Unmarshal([]byte(out), &res) != nil {
		return 0
	}
	return res.CPU.Total
}

// Resolve is ref's fingerprint: a pats-img- alias or a fingerprint in the
// pats project, an image remote's alias (ubuntu:26.04) as it names one now.
func (l *lxd) Resolve(ctx context.Context, ref string) (Image, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return Image{}, err
	}
	fp, err := c.fingerprint(ctx, ref)
	if err != nil {
		return Image{}, noImage(err)
	}
	return Image{ID: fp, Name: ref}, nil
}

func (l *lxd) Images(ctx context.Context) (map[string]bool, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return nil, err
	}
	var ims []struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := c.query(ctx, "/1.0/images?project="+lxdProject+"&recursion=1", &ims); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, im := range ims {
		out[im.Fingerprint] = true
	}
	return out, nil
}

// Pull copies ref, an image on another remote, into the pats project; the
// image is what it copied, which a later Resolve of ref (asking the image
// remote) may not be. a ref on the server itself is only resolved.
func (l *lxd) Pull(ctx context.Context, ref string, force bool, _ io.Writer) (Image, bool, error) {
	if !strings.Contains(ref, ":") {
		img, err := l.Resolve(ctx, ref)
		return img, false, err
	}
	c, err := l.ready(ctx)
	if err != nil {
		return Image{}, false, err
	}
	fp, copied, err := c.copyImage(ctx, ref, force)
	if err != nil {
		return Image{}, false, err
	}
	return Image{ID: fp, Name: ref}, copied, nil
}

// Build provisions one layer on spec.Base and caches it under a
// pats-img-<key> alias; the base returned is the fingerprint the key was
// made of.
func (l *lxd) Build(ctx context.Context, spec BuildSpec, log io.Writer) (Image, string, bool, error) {
	if spec.Dockerfile != "" {
		return Image{}, "", false, errors.New("lxd builds from a base and a provision step, not a dockerfile")
	}
	if spec.Base == "" {
		return Image{}, "", false, errors.New("lxd: a build needs a base image")
	}
	c, err := l.ready(ctx)
	if err != nil {
		return Image{}, "", false, err
	}
	if log == nil {
		log = io.Discard
	}
	alias, fp, base, built, err := c.image(ctx, lxdImageSpec{Base: spec.Base, Material: spec.Material, Argv: spec.Argv, Stamp: spec.Stamp, Harness: spec.Harness,
		Progress: spec.Progress, Warn: spec.Warn, Role: spec.Role, Sandbox: spec.Sandbox, Force: spec.Force}, log)
	if err != nil {
		return Image{}, "", false, err
	}
	return Image{ID: fp, Name: alias}, base, built, nil
}

// ProxyImage builds the egress proxy image on the server, once per process
// unless force; ref is the container drivers' and unused here.
func (l *lxd) ProxyImage(ctx context.Context, _ string, force bool, log io.Writer) (Image, bool, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return Image{}, false, err
	}
	c.s = SandboxOptions{}
	alias, built, err := c.proxyImage(ctx, force, log)
	if err != nil {
		return Image{}, false, err
	}
	fp, err := c.fingerprint(ctx, alias)
	if err != nil {
		return Image{}, false, err
	}
	return Image{ID: fp, Name: alias}, built, nil
}

func (l *lxd) Stamps(ctx context.Context, id string, clis []string) (map[string]string, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return nil, err
	}
	stamps, err := c.imageStamps(ctx, id, clis)
	return stamps, noImage(err)
}

func (l *lxd) RemoveImage(ctx context.Context, id string) error {
	c, err := l.ready(ctx)
	if err != nil {
		return err
	}
	if _, err := c.lxc(ctx, "image", "delete", c.ref(id), "--project", lxdProject); err != nil && !notFound(err) {
		return err
	}
	return nil
}

// Upstream is the fingerprint an image remote's alias names now for this
// server's architecture and instance type.
// Arch is the server's: an image there runs on it.
func (l *lxd) Arch(ctx context.Context, _ string) (string, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return "", err
	}
	return c.serverArch(ctx)
}

func (l *lxd) Upstream(ctx context.Context, ref string) (string, error) {
	return l.remoteFingerprint(ctx, ref)
}

// Usage is what the server records of image id's use.
func (l *lxd) Usage(ctx context.Context, id string) (ImageUse, error) {
	c, err := l.ready(ctx)
	if err != nil {
		return ImageUse{}, err
	}
	return c.imageUsage(ctx, id)
}

var (
	_ Driver     = (*lxd)(nil)
	_ ImageUsage = (*lxd)(nil)
	_ Driver     = (*container)(nil)
)
