package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/images"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// usedSandboxes resolves each pair's agent to its sandbox id and returns the
// set actually needed by this run -- shared by buildImages and
// pullEgressImages so both skip sandboxes no pair references. an agent whose
// sandbox fails to resolve is silently skipped here; ResolveSandbox's error
// surfaces later, in runPair, where it can be attributed to that pair.
func usedSandboxes(cfg *config.Config, pairs []config.TestPair) map[string]bool {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	used := map[string]bool{}
	for _, p := range pairs {
		if id, err := cfg.ResolveSandbox(agents[p.Agent]); err == nil {
			used[id] = true
		}
	}
	return used
}

// provisions reports whether a sandbox makes its image from base / provision.
func provisions(sb config.Sandbox) bool {
	return sb.Base != "" || sb.Provision != ""
}

// buildLog is where a build or pull writes: its log, and under -vv the
// terminal too.
func (o Options) buildLog(log io.Writer) io.Writer {
	if s := o.stream(); s != nil {
		return io.MultiWriter(log, s)
	}
	return log
}

// lxdImage provisions an lxd sandbox's image on h: with no base, the pats
// sandbox's tools on ubuntu first, then the sandbox's own provision on top --
// two cached images in a chain, the first's alias in the second's key. the
// builds log to logPath. layers are the fingerprints under the image, bottom
// first. upstream is each base on an image remote, and the fingerprint the
// build keyed on: what a run later checks upstream.
func lxdImage(ctx context.Context, h builder, sb config.Sandbox, opts Options, logPath string, force bool) (img sandbox.Image, layers []string, built bool, upstream map[string]string, err error) {
	upstream = map[string]string{}
	based := func(ref, fp string) {
		if strings.Contains(ref, ":") && !strings.HasPrefix(ref, "pats-img-") {
			upstream[ref] = fp
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return img, nil, false, nil, err
	}
	logF, err := os.Create(logPath)
	if err != nil {
		return img, nil, false, nil, err
	}
	defer logF.Close()
	log := opts.buildLog(logF)
	lg := logw{opts.Out, opts.Color, opts.Verbose}
	progress := func(msg string) { lg.detail("sandbox %q: %s", sb.ID, msg) }
	warn := func(msg string) { lg.warn("sandbox %q: %s", sb.ID, msg) }
	fail := func(err error) error {
		return fmt.Errorf("%w%s\n(full log: %s)", err, sandbox.LogTail(fileTail(logPath, 64<<10)), relToCwd(logPath))
	}
	base, anyBuilt := sb.Base, false
	if base == "" {
		material, argv, err := stageMaterial(opts.ConfigDir, images.ProvisionSandbox, sandbox.ProvisionMount)
		if err != nil {
			return img, nil, false, nil, err
		}
		defer sandbox.RemoveTemp(material)
		// the script reads its pins from beside itself; in the material they
		// are also in the image key, so a pin bump reprovisions.
		if err := os.WriteFile(filepath.Join(material, "harness-versions"), []byte(images.HarnessVersions), 0o644); err != nil {
			return img, nil, false, nil, err
		}
		layer, fp, b, err := h.Build(ctx, sandbox.BuildSpec{Base: lxdSandboxBase, Material: material, Argv: argv, Progress: progress, Warn: warn,
			Role: "sandbox", Sandbox: sb.ID, Force: force}, log)
		if err != nil {
			return img, nil, false, nil, fail(err)
		}
		based(lxdSandboxBase, fp)
		base, anyBuilt = layer.Name, b
		layers = append(layers, layer.ID)
		img = layer
	}
	spec := sandbox.BuildSpec{Base: base, Progress: progress, Warn: warn, Role: "project", Sandbox: sb.ID, Force: force}
	if sb.Provision != "" {
		material, argv, err := stageMaterial(opts.ConfigDir, sb.Provision, sandbox.ProvisionMount)
		if err != nil {
			return img, nil, false, nil, err
		}
		defer sandbox.RemoveTemp(material)
		spec.Material, spec.Argv = material, argv
	} else if len(layers) > 0 {
		// no project layer: the pats sandbox layer is the image.
		return img, nil, anyBuilt, upstream, nil
	}
	img, fp, built, err := h.Build(ctx, spec, log)
	if err != nil {
		return img, nil, false, nil, fail(err)
	}
	based(base, fp)
	return img, layers, built || anyBuilt, upstream, nil
}

// fileTail is up to n bytes off the end of a file, "" when it can't be read.
func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

// lxdSandboxBase is what the pats sandbox is provisioned on under lxd: the
// release the published docker sandbox image is built from.
const lxdSandboxBase = "ubuntu:26.04"

// resolveBuildSpec turns a sandbox `build:` value into (context dir,
// dockerfile). spec is a dir (context, with a Dockerfile inside) or a
// Dockerfile path (context = its dir), relative to configDir.
func resolveBuildSpec(configDir, spec string) (ctxDir, dockerfile string, err error) {
	p := spec
	if !filepath.IsAbs(p) {
		p = filepath.Join(configDir, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", "", fmt.Errorf("build context: %w", err)
	}
	if fi.IsDir() {
		return p, filepath.Join(p, "Dockerfile"), nil
	}
	return filepath.Dir(p), p, nil
}

// proxyImageID names the sidecar image a proxy-mode sandbox ran: its registry
// digest (repo@sha256:...) when it was pulled, else the local image id (a
// locally built image has no registry digest). "" when neither can be read.
func proxyImageID(ctx context.Context, sb config.Sandbox) string {
	if !sb.IsContainer() {
		return ""
	}
	h, err := hostFor(sb)
	if err != nil {
		return ""
	}
	img, err := h.Resolve(ctx, sb.ProxyImage)
	switch {
	case err != nil:
		return ""
	case img.Digest != "":
		return img.Repo + "@" + img.Digest
	}
	return img.ID
}
