package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// defaultImages fills in the published pats sandbox, pinned to this build's
// version, for every used sandbox that names neither image nor build. the
// version warning (dev build -> :latest) is logged once.
func defaultImages(cfg *config.Config, opts Options, pairs []config.TestPair) {
	used := usedSandboxes(cfg, pairs)
	lg := logw{opts.Out, opts.Color}
	warned := false
	for i, sb := range cfg.Sandboxes {
		if !used[sb.ID] || sb.Image != "" || sb.Build != "" || sb.Driver == "lxd" || provisions(sb) {
			continue
		}
		img, warn := sandbox.SandboxImage("")
		if warn != "" && !warned {
			lg.warn("%s", warn)
			warned = true
		}
		cfg.Sandboxes[i].Image = img
	}
}

// buildImages resolves the `build:` sandboxes used by the run's pairs: each
// context is hashed first and an image built from identical inputs is reused
// outright (see buildContextHash); otherwise it's built via its driver
// (`build -q`). the resulting image id is written back onto cfg as the
// sandbox's image, so the rest of the run -- preflight included -- just sees
// an image. the id also lands in each pair's metadata.json, recording exactly
// what ran.
func buildImages(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair, runDir string) error {
	used := usedSandboxes(cfg, pairs)
	lg := logw{opts.Out, opts.Color}
	warned := false
	base := func() string {
		img, warn := sandbox.SandboxImage("")
		if warn != "" && !warned {
			lg.warn("%s", warn)
			warned = true
		}
		return img
	}
	for i, sb := range cfg.Sandboxes {
		if !used[sb.ID] {
			continue
		}
		var id string
		var built bool
		var err error
		switch {
		case sb.Driver == "lxd" && sb.Image == "":
			id, built, err = lxdImage(ctx, sb, opts, runDir)
		case sb.IsContainer() && provisions(sb):
			id, built, err = provisionedContainerImage(ctx, sb, opts, base)
		case sb.Build != "":
			var ctxDir, dockerfile string
			if ctxDir, dockerfile, err = resolveBuildSpec(opts.ConfigDir, sb.Build); err == nil {
				id, built, err = sandbox.BuildContainerImage(ctx, sb.Driver, sandbox.ContainerBuild{Dir: ctxDir, Dockerfile: dockerfile}, base)
			}
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		if built {
			lg.info("built sandbox %q image: %s", sb.ID, id)
		} else {
			lg.info("sandbox %q image unchanged -- reusing %s", sb.ID, id)
		}
		cfg.Sandboxes[i].Image = id
	}
	return nil
}

// provisions reports whether a sandbox makes its image from base / provision.
func provisions(sb config.Sandbox) bool {
	return sb.Base != "" || sb.Provision != "" || sb.ProvisionCmd != ""
}

// lxdImage provisions an lxd sandbox's image: with no base, the pats
// sandbox's tools on ubuntu first, then the sandbox's own provision on top --
// two cached images in a chain, the first's alias in the second's key. the
// builds log to <run>/images/<sandbox>.log.
func lxdImage(ctx context.Context, sb config.Sandbox, opts Options, runDir string) (string, bool, error) {
	if err := os.MkdirAll(filepath.Join(runDir, "images"), 0o755); err != nil {
		return "", false, err
	}
	logPath := filepath.Join(runDir, "images", sb.ID+".log")
	logF, err := os.Create(logPath)
	if err != nil {
		return "", false, err
	}
	defer logF.Close()
	o := sandbox.Options{Driver: "lxd", Remote: sb.Remote, VM: sb.VM}
	lg := logw{opts.Out, opts.Color}
	progress := func(msg string) { lg.info("sandbox %q: %s", sb.ID, msg) }
	warn := func(msg string) { lg.warn("sandbox %q: %s", sb.ID, msg) }
	fail := func(err error) error {
		return fmt.Errorf("%w%s\n(full log: %s)", err, sandbox.LogTail(fileTail(logPath, 64<<10)), relToCwd(logPath))
	}
	base, anyBuilt := sb.Base, false
	if base == "" {
		material, argv, err := stageMaterial(opts.ConfigDir, opts.ConfigDir, images.ProvisionSandbox, "", sandbox.ProvisionMount)
		if err != nil {
			return "", false, err
		}
		defer sandbox.RemoveTemp(material)
		base, anyBuilt, err = sandbox.LXDImage(ctx, o, sandbox.LXDImageSpec{Base: lxdSandboxBase, Material: material, Argv: argv, Progress: progress, Warn: warn}, logF)
		if err != nil {
			return "", false, fail(err)
		}
	}
	spec := sandbox.LXDImageSpec{Base: base, Progress: progress, Warn: warn}
	if sb.Provision != "" || sb.ProvisionCmd != "" {
		material, argv, err := stageMaterial(opts.ConfigDir, opts.ConfigDir, sb.Provision, sb.ProvisionCmd, sandbox.ProvisionMount)
		if err != nil {
			return "", false, err
		}
		defer sandbox.RemoveTemp(material)
		spec.Material, spec.Argv = material, argv
	}
	alias, built, err := sandbox.LXDImage(ctx, o, spec, logF)
	if err != nil {
		return "", false, fail(err)
	}
	return alias, built || anyBuilt, nil
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

// provisionedContainerImage builds base / provision for docker or podman: a
// generated dockerfile running the provision step on base (the pats sandbox
// when empty), through the same cached build as build:.
func provisionedContainerImage(ctx context.Context, sb config.Sandbox, opts Options, base func() string) (string, bool, error) {
	provision := sb.Provision != "" || sb.ProvisionCmd != ""
	var material string
	var argv []string
	if provision {
		var err error
		if material, argv, err = stageMaterial(opts.ConfigDir, opts.ConfigDir, sb.Provision, sb.ProvisionCmd, sandbox.ProvisionMount); err != nil {
			return "", false, err
		}
		defer sandbox.RemoveTemp(material)
	}
	from := sb.Base
	if from == "" {
		from = "${PATS_SANDBOX}"
	}
	var df strings.Builder
	df.WriteString("ARG PATS_SANDBOX\n")
	fmt.Fprintf(&df, "FROM %s\n", from)
	df.WriteString("USER root\n")
	if provision {
		fmt.Fprintf(&df, "COPY material %s\n", sandbox.ProvisionMount)
		run, _ := json.Marshal(inPath(sandbox.ProvisionMount, argv))
		fmt.Fprintf(&df, "RUN %s\n", run)
		fmt.Fprintf(&df, "RUN rm -rf %s\n", sandbox.ProvisionMount)
	}
	ctxDir, err := sandbox.MkTemp("pats-build-")
	if err != nil {
		return "", false, err
	}
	defer sandbox.RemoveTemp(ctxDir)
	if provision {
		if err := os.Rename(material, filepath.Join(ctxDir, "material")); err != nil {
			return "", false, err
		}
	}
	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(df.String()), 0o644); err != nil {
		return "", false, err
	}
	return sandbox.BuildContainerImage(ctx, sb.Driver, sandbox.ContainerBuild{Dir: ctxDir, Dockerfile: dockerfile}, base)
}

// pullEgressImages pre-pulls the egress proxy image for any proxy-mode sandbox
// the run uses, so the per-pair (and preflight) proxy start finds it cached --
// otherwise the first `docker run` of the proxy pulls silently, stalling the
// run with no output. only images absent locally are pulled: a present one is
// left alone (no registry round-trip every run).
func pullEgressImages(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair) error {
	used := usedSandboxes(cfg, pairs)
	lg := logw{opts.Out, opts.Color}
	seen := map[string]bool{}
	for _, sb := range cfg.Sandboxes {
		// lxd provisions its proxy image itself, on the server.
		if !used[sb.ID] || !sb.Egress.Set || !sb.IsContainer() {
			continue
		}
		driver := sb.Driver
		img, warn := sandbox.ProxyImage(sb.Egress.Image)
		key := driver + "\x00" + img
		if seen[key] {
			continue
		}
		seen[key] = true
		if warn != "" {
			lg.warn("%s", warn)
		}
		if exec.CommandContext(ctx, driver, "image", "inspect", img).Run() == nil {
			continue // already local -- skip the pull + its round-trip
		}
		lg.info("pulling egress image %q...", img)
		if out, err := exec.CommandContext(ctx, driver, "pull", img).CombinedOutput(); err != nil {
			return fmt.Errorf("pull egress image %q: %w\n%s", img, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

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
	img, _ := sandbox.ProxyImage(sb.Egress.Image)
	// docker and podman both expose RepoDigests and Id on `image inspect`.
	out, err := exec.CommandContext(ctx, sb.Driver, "image", "inspect", "--format",
		"{{range .RepoDigests}}{{.}} {{end}}|{{.Id}}", img).Output()
	if err != nil {
		return ""
	}
	digests, id, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	if fields := strings.Fields(digests); len(fields) > 0 {
		return fields[0]
	}
	return strings.TrimSpace(id)
}
