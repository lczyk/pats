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

// provisions reports whether a sandbox makes its image from base / provision.
func provisions(sb config.Sandbox) bool {
	return sb.Base != "" || sb.Provision != "" || sb.ProvisionCmd != ""
}

// lxdImageFn is sandbox.LXDImage, a var like newSandbox, so the build's
// progress, warnings and errors are tested without a server.
var lxdImageFn = sandbox.LXDImage

// lxdImage provisions an lxd sandbox's image: with no base, the pats
// sandbox's tools on ubuntu first, then the sandbox's own provision on top --
// two cached images in a chain, the first's alias in the second's key. the
// builds log to logPath. layers are the aliases under the image, bottom
// first.
func lxdImage(ctx context.Context, sb config.Sandbox, opts Options, logPath string, force bool) (alias string, layers []string, built bool, upstream map[string]string, err error) {
	// the bottom layer's base on an image remote, and the fingerprint the
	// build keyed on: what a run later checks upstream.
	upstream = map[string]string{}
	based := func(ref string) func(string) {
		if !strings.Contains(ref, ":") || strings.HasPrefix(ref, "pats-img-") {
			return nil
		}
		return func(fp string) { upstream[ref] = fp }
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", nil, false, nil, err
	}
	logF, err := os.Create(logPath)
	if err != nil {
		return "", nil, false, nil, err
	}
	defer logF.Close()
	o := sandbox.Options{Driver: "lxd", Remote: sb.Remote, VM: sb.VM}.WithEnv()
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
			return "", nil, false, nil, err
		}
		defer sandbox.RemoveTemp(material)
		// the script reads its pins from beside itself; in the material they
		// are also in the image key, so a pin bump reprovisions.
		if err := os.WriteFile(filepath.Join(material, "harness-versions"), []byte(images.HarnessVersions), 0o644); err != nil {
			return "", nil, false, nil, err
		}
		base, anyBuilt, err = lxdImageFn(ctx, o, sandbox.LXDImageSpec{Base: lxdSandboxBase, Material: material, Argv: argv, Progress: progress, Warn: warn, Based: based(lxdSandboxBase),
			Role: "sandbox", Sandbox: sb.ID, Force: force}, logF)
		if err != nil {
			return "", nil, false, nil, fail(err)
		}
		layers = append(layers, base)
	}
	spec := sandbox.LXDImageSpec{Base: base, Progress: progress, Warn: warn, Based: based(base), Role: "project", Sandbox: sb.ID, Force: force}
	if sb.Provision != "" || sb.ProvisionCmd != "" {
		material, argv, err := stageMaterial(opts.ConfigDir, opts.ConfigDir, sb.Provision, sb.ProvisionCmd, sandbox.ProvisionMount)
		if err != nil {
			return "", nil, false, nil, err
		}
		defer sandbox.RemoveTemp(material)
		spec.Material, spec.Argv = material, argv
	} else if len(layers) > 0 {
		// no project layer: the pats sandbox layer is the image.
		return base, nil, anyBuilt, upstream, nil
	}
	alias, built, err = lxdImageFn(ctx, o, spec, logF)
	if err != nil {
		return "", nil, false, nil, fail(err)
	}
	return alias, layers, built || anyBuilt, upstream, nil
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
func provisionedContainerImage(ctx context.Context, sb config.Sandbox, opts Options, base func() string, force bool, log io.Writer) (string, bool, error) {
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
	return sandbox.BuildContainerImage(ctx, sb.Driver, sandbox.ContainerBuild{Dir: ctxDir, Dockerfile: dockerfile, Force: force, Log: log}, base)
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
	img := sb.ProxyImage
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
