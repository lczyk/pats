package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
		if !used[sb.ID] || sb.Image != "" || sb.Build != "" {
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
func buildImages(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair) error {
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
		if !used[sb.ID] || sb.Build == "" {
			continue
		}
		ctxDir, dockerfile, err := resolveBuildSpec(opts.ConfigDir, sb.Build)
		if err != nil {
			return fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		id, built, err := sandbox.BuildContainerImage(ctx, sb.Driver, sandbox.ContainerBuild{Dir: ctxDir, Dockerfile: dockerfile}, base)
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
		if !used[sb.ID] || !sb.Egress.Set {
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
