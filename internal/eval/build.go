package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// buildImages resolves the `build:` sandboxes used by the run's pairs: each
// context is hashed first and an image built from identical inputs is reused
// outright (see buildContextHash); otherwise it's built via its driver
// (`build -q`). the resulting image id is written back onto cfg as the
// sandbox's image, so the rest of the run -- preflight included -- just sees
// an image. the id also lands in each pair's metadata.json, recording exactly
// what ran.
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
		driver := sb.ResolvedDriver()
		ctxDir, dockerfile, err := resolveBuildSpec(opts.ConfigDir, sb.Build)
		if err != nil {
			return fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		if err := checkPatsIgnored(ctxDir, dockerfile); err != nil {
			return fmt.Errorf("sandbox %q: %w", sb.ID, err)
		}
		// the dry cache check: hash the build inputs and look for an image
		// already labelled with that hash -- a hit skips `docker build`
		// entirely instead of rebuilding and hoping the layer cache holds.
		hash, err := buildContextHash(ctxDir, dockerfile)
		if err != nil {
			hash = "" // fs hiccup -> just build; the build will surface the real error
		}
		if id := cachedImageID(ctx, driver, hash); id != "" {
			lg.info("sandbox %q image unchanged -- reusing %s", sb.ID, id)
			cfg.Sandboxes[i].Image = id
			continue
		}
		lg.info("building sandbox %q image (%s)... (first build or context changed)", sb.ID, sb.Build)
		id, err := buildImage(ctx, driver, ctxDir, dockerfile, hash)
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

// hashLabel is the image label carrying the build-input hash; cachedImageID
// filters on it to answer "would this build be a full cache hit?" without
// running the build.
const hashLabel = "pats.context-hash"

// buildImage builds one resolved context and returns the image id. a non-empty
// hash is stamped onto the image (see hashLabel) so the next run can find it.
func buildImage(ctx context.Context, driver, ctxDir, dockerfile, hash string) (string, error) {
	args := []string{"build", "-q", "-f", dockerfile}
	if hash != "" {
		args = append(args, "--label", hashLabel+"="+hash)
	}
	cmd := exec.CommandContext(ctx, driver, append(args, ctxDir)...)
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

// cachedImageID looks up an image already built from identical inputs (same
// hashLabel value); "" means none and the caller must build. reuse carries the
// same staleness the layer cache always had: a mutable FROM tag or a RUN that
// fetches "latest" won't refresh until the dockerfile or context changes (or
// the image is removed -- `docker rmi` is the force-rebuild escape hatch).
func cachedImageID(ctx context.Context, driver, hash string) string {
	if hash == "" {
		return ""
	}
	// -a: `build -q` leaves the image untagged, which plain `images` hides.
	out, err := exec.CommandContext(ctx, driver, "images", "-a", "--no-trunc", "-q",
		"--filter", "label="+hashLabel+"="+hash).Output()
	if err != nil {
		return ""
	}
	ids := strings.Fields(string(out)) // newest first; older builds may share the label
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// buildContextHash digests everything that determines a build's outcome from
// the host side: the dockerfile, the effective dockerignore, and -- only when
// the dockerfile actually reads its context -- every non-ignored file in it
// (path, mode, symlink target, content). two equal hashes mean docker would
// hit its layer cache the whole way down, so the build can be skipped without
// running it. a context-less dockerfile (no COPY/ADD) hashes the dockerfile
// alone -- context churn (e.g. a growing .pats/) can't invalidate anything.
//
// the dockerignore matcher is approximate (see matchIgnore); it errs toward
// hashing too much, which at worst degrades to today's behaviour: an
// unnecessary `docker build` that the layer cache absorbs.
func buildContextHash(ctxDir, dockerfile string) (string, error) {
	h := sha256.New()
	df, err := os.ReadFile(dockerfile)
	if err != nil {
		return "", err
	}
	h.Write(df)

	var patterns []ignorePattern
	if data, err := os.ReadFile(effectiveIgnoreFile(ctxDir, dockerfile)); err == nil {
		h.Write(data)
		patterns = parseIgnore(string(data))
	}

	if usesContext(dockerfile) {
		if err := hashContextDir(h, ctxDir, patterns); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashContextDir feeds every non-ignored entry under ctxDir into h in walk
// (lexical, deterministic) order.
func hashContextDir(h io.Writer, ctxDir string, patterns []ignorePattern) error {
	// with negations present a pruned dir could hide re-included children, so
	// only prune when there are none.
	canPrune := true
	for _, p := range patterns {
		if p.negate {
			canPrune = false
		}
	}
	return filepath.WalkDir(ctxDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(ctxDir, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if ignored(patterns, rel) {
			if d.IsDir() && canPrune {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			fmt.Fprintf(h, "D %s\n", rel)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "L %s -> %s\n", rel, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "F %s %o %d\n", rel, info.Mode().Perm(), info.Size())
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
		default:
			// sockets/fifos/devices: docker refuses or skips these anyway;
			// record their presence and move on.
			fmt.Fprintf(h, "S %s\n", rel)
		}
		return nil
	})
}

// effectiveIgnoreFile returns the ignore file docker would consult: buildkit's
// <Dockerfile-name>.dockerignore fully replaces .dockerignore when present.
func effectiveIgnoreFile(ctxDir, dockerfile string) string {
	perDF := filepath.Join(ctxDir, filepath.Base(dockerfile)+".dockerignore")
	if _, err := os.Stat(perDF); err == nil {
		return perDF
	}
	return filepath.Join(ctxDir, ".dockerignore")
}

// ignorePattern is one parsed dockerignore line.
type ignorePattern struct {
	pattern string // cleaned, slash-separated, no leading "/" or "./"
	negate  bool
}

// parseIgnore parses a dockerignore body: comments and blanks dropped,
// leading "!" marks a negation, paths normalised to the matcher's shape.
func parseIgnore(body string) []ignorePattern {
	var out []ignorePattern
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		neg := strings.HasPrefix(l, "!")
		l = strings.TrimPrefix(l, "!")
		l = strings.TrimPrefix(l, "./")
		l = strings.TrimPrefix(l, "/")
		l = strings.TrimSuffix(l, "/")
		if l == "" {
			continue
		}
		out = append(out, ignorePattern{pattern: path.Clean(l), negate: neg})
	}
	return out
}

// ignored applies dockerignore semantics to one slash-separated relative
// path: the last matching pattern wins; a pattern matching a parent directory
// covers everything beneath it.
//
// matching is a deliberate approximation of docker's (per-segment globs plus
// "**" spanning any depth). exotic patterns a fancier matcher would catch fall
// through to "not ignored" -- the safe direction: the file gets hashed, and a
// spurious mismatch just means an unnecessary (cheap, layer-cached) build.
func ignored(patterns []ignorePattern, rel string) bool {
	ig := false
	for _, p := range patterns {
		if matchIgnore(p.pattern, rel) {
			ig = !p.negate
		}
	}
	return ig
}

// matchIgnore reports whether pattern covers rel or one of rel's ancestors.
func matchIgnore(pattern, rel string) bool {
	for r := rel; r != "."; r = path.Dir(r) {
		if segmentsMatch(strings.Split(pattern, "/"), strings.Split(r, "/")) {
			return true
		}
	}
	return false
}

// segmentsMatch matches pattern segments against path segments; "**" spans
// zero or more segments, everything else is a per-segment glob (path.Match).
func segmentsMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if segmentsMatch(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
		return false
	}
	return segmentsMatch(pat[1:], segs[1:])
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
	ignore := effectiveIgnoreFile(ctxDir, dockerfile)
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
