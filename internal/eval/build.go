package eval

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/sandbox"
)

// buildImages resolves the `build:` sandboxes used by the run's pairs: each is
// built via its driver (`build -q`; the layer cache makes no-change rebuilds
// cheap) and the resulting image id is written back onto cfg as the sandbox's
// image, so the rest of the run -- preflight included -- just sees an image.
// the id also lands in each pair's metadata.json, recording exactly what ran.
func buildImages(ctx context.Context, cfg *config.Config, opts Options, pairs []config.TestPair) error {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	used := map[string]bool{}
	for _, p := range pairs {
		if id, err := cfg.ResolveSandbox(agents[p.Agent]); err == nil {
			used[id] = true
		}
	}
	lg := logw{opts.Out, opts.Color}
	for i, sb := range cfg.Sandboxes {
		if !used[sb.ID] || sb.Build == "" {
			continue
		}
		lg.info("building sandbox %q image (%s)...", sb.ID, sb.Build)
		id, err := buildImage(ctx, sb.ResolvedDriver(), opts.ConfigDir, sb.Build)
		if err != nil {
			return fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		lg.info("built sandbox %q image: %s", sb.ID, id)
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
	agents := index(cfg.Agents, func(a config.Agent) string { return a.ID })
	used := map[string]bool{}
	for _, p := range pairs {
		if id, err := cfg.ResolveSandbox(agents[p.Agent]); err == nil {
			used[id] = true
		}
	}
	lg := logw{opts.Out, opts.Color}
	seen := map[string]bool{}
	for _, sb := range cfg.Sandboxes {
		if !used[sb.ID] || (sb.Egress.Mode != "proxy" && sb.Egress.Mode != "mitm-proxy") {
			continue
		}
		driver := sb.ResolvedDriver()
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

// buildImage builds one context and returns the image id. spec is a dir
// (context, with a Dockerfile inside) or a Dockerfile path (context = its
// dir), relative to configDir.
func buildImage(ctx context.Context, driver, configDir, spec string) (string, error) {
	p := spec
	if !filepath.IsAbs(p) {
		p = filepath.Join(configDir, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}
	ctxDir, dockerfile := p, filepath.Join(p, "Dockerfile")
	if !fi.IsDir() {
		ctxDir, dockerfile = filepath.Dir(p), p
	}
	if err := checkPatsIgnored(ctxDir, dockerfile); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, driver, "build", "-q", "-f", dockerfile, ctxDir)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s build %s: %w%s", driver, ctxDir, err, stderrTail(&errb))
	}
	id := strings.TrimSpace(out.String())
	if id == "" {
		return "", fmt.Errorf("%s build %s: no image id on stdout", driver, ctxDir)
	}
	return id, nil
}

// checkPatsIgnored refuses to build a context containing a .pats dir (run
// artifacts -- can be huge, would leak past run outputs into the image, and
// invalidate the layer cache every run) unless the effective dockerignore
// excludes it. only enforced when the dockerfile actually reads the context
// (a COPY/ADD not sourced --from another stage) -- a context-less build has
// nothing to leak. pats deliberately does not write or patch ignore files in
// the user's tree -- the fix is one versioned line in their repo.
func checkPatsIgnored(ctxDir, dockerfile string) error {
	if _, err := os.Stat(filepath.Join(ctxDir, ".pats")); err != nil {
		return nil // no .pats in the context -- nothing to leak
	}
	if !usesContext(dockerfile) {
		return nil
	}
	// buildkit precedence: <Dockerfile-name>.dockerignore, if present, fully
	// replaces .dockerignore for that build.
	ignore := filepath.Join(ctxDir, filepath.Base(dockerfile)+".dockerignore")
	if _, err := os.Stat(ignore); err != nil {
		ignore = filepath.Join(ctxDir, ".dockerignore")
	}
	if data, err := os.ReadFile(ignore); err == nil && ignoresPats(string(data)) {
		return nil
	}
	return fmt.Errorf("build context %s contains .pats/ (run artifacts) and %s does not exclude it -- add a `.pats/` line", ctxDir, filepath.Base(ignore))
}

// usesContext reports whether a dockerfile reads its build context: any
// COPY/ADD instruction not sourced from another stage (--from=). the scan is
// line-based and errs on the safe side -- an unreadable dockerfile, or one
// with e.g. a heredoc body whose lines look like instructions, counts as
// using the context (worst case the .pats guard fires unnecessarily).
func usesContext(dockerfile string) bool {
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if (strings.HasPrefix(l, "copy ") || strings.HasPrefix(l, "add ")) &&
			!strings.Contains(l, "--from=") {
			return true
		}
	}
	return false
}

// ignoresPats reports whether a dockerignore body has a line covering .pats.
// matching is deliberately dumb: a line that normalises to ".pats" (optional
// leading "./" or "/", optional trailing "/" or "/**"). a fancier glob that
// happens to cover .pats is not recognised -- write the literal line.
func ignoresPats(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimPrefix(l, "./")
		l = strings.TrimPrefix(l, "/")
		l = strings.TrimSuffix(l, "/**")
		l = strings.TrimSuffix(l, "/")
		if l == ".pats" {
			return true
		}
	}
	return false
}
