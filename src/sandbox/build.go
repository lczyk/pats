package sandbox

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
)

// ContainerBuild is the docker build context a sandbox names with build:.
type ContainerBuild struct {
	Dir        string // the context
	Dockerfile string
}

// BuildContainerImage resolves a build: sandbox to an image id for a
// container driver: an image already built from identical inputs (the
// dockerfile, the context it reads, the id of every FROM image) when one
// exists, else a fresh build. base gives the pats sandbox image, asked for
// only when the dockerfile names PATS_SANDBOX, and handed in as that build
// arg. built reports whether a build ran.
func BuildContainerImage(ctx context.Context, driver string, b ContainerBuild, base func() string) (id string, built bool, err error) {
	// read once, shared by checkPatsIgnored (via usesContext) and
	// buildContextHash below.
	dfData, dfErr := os.ReadFile(b.Dockerfile)
	if err := checkPatsIgnored(b.Dir, b.Dockerfile, dfData, dfErr); err != nil {
		return "", false, err
	}
	// the dry cache check: hash the build inputs and look for an image
	// already labelled with that hash -- a hit skips the build entirely
	// instead of rebuilding and hoping the layer cache holds.
	baseImg := ""
	if bytes.Contains(dfData, []byte(baseArg)) {
		baseImg = base()
	}
	hash, err := buildContextHash(b.Dir, b.Dockerfile, dfData, dfErr, baseImg)
	if err != nil {
		hash = "" // fs hiccup -> just build; the build will surface the real error
	}
	if hash != "" {
		refs, exact := fromRefs(dfData, baseImg)
		if !exact {
			hash = "" // the key could not name the base the build uses: build, the layer cache decides
		} else {
			ids, err := baseImageIDs(ctx, driver, refs)
			if err != nil {
				return "", false, err
			}
			hash = rehash(hash, ids)
		}
	}
	if id := cachedImageID(ctx, driver, hash); id != "" {
		return id, false, nil
	}
	id, err = buildImage(ctx, driver, b.Dir, b.Dockerfile, hash, baseImg)
	return id, err == nil, err
}

// stderrTail renders a failed command's captured stderr for embedding in an
// error: the last few lines, enough to see the crash.
// LogTail is a build log's last lines, for an error: what failed is usually
// at the end, and the whole log can run to megabytes.
func LogTail(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	const keep, width = 15, 240
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	for i, l := range lines {
		if len(l) > width {
			lines[i] = l[:width] + "..."
		}
	}
	return "\nlog tail:\n  " + strings.Join(lines, "\n  ")
}

func stderrTail(b *bytes.Buffer) string {
	s := strings.TrimSpace(b.String())
	if s == "" {
		return ""
	}
	const keep = 10
	if lines := strings.Split(s, "\n"); len(lines) > keep {
		s = "... " + strings.Join(lines[len(lines)-keep:], "\n")
	}
	return "\nstderr: " + s
}

// hashLabel is the image label carrying the build-input hash; cachedImageID
// filters on it to answer "would this build be a full cache hit?" without
// running the build.
const hashLabel = "pats.context-hash"

// baseArg is the build arg carrying the pats sandbox image pinned to this
// version, so a context can `ARG PATS_SANDBOX` / `FROM ${PATS_SANDBOX}` and
// track the pats it runs under.
const baseArg = "PATS_SANDBOX"

// buildImage builds one resolved context and returns the image id. a non-empty
// hash is stamped onto the image (see hashLabel) so the next run can find it;
// a non-empty base is handed to the build as PATS_SANDBOX (see baseArg).
func buildImage(ctx context.Context, driver, ctxDir, dockerfile, hash, base string) (string, error) {
	args := []string{"build", "-q", "-f", dockerfile}
	if base != "" {
		args = append(args, "--build-arg", baseArg+"="+base)
	}
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

// fromRefs lists the images a dockerfile builds FROM, the pats sandbox arg
// and the defaults of ARGs declared before the first FROM substituted, earlier
// stages and scratch skipped. exact is false when a ref still names an arg it
// cannot resolve, or a FROM picks a --platform: the key could then name a
// different image than the build uses, and the caller skips the dry cache.
func fromRefs(dfData []byte, base string) (refs []string, exact bool) {
	exact = true
	args := map[string]string{baseArg: base}
	stages := map[string]bool{}
	seenFrom := false
	// a backslash ends a line that continues on the next.
	joined := strings.ReplaceAll(strings.ReplaceAll(string(dfData), "\\\r\n", " "), "\\\n", " ")
	for _, line := range strings.Split(joined, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if strings.EqualFold(f[0], "ARG") && !seenFrom {
			if name, def, ok := strings.Cut(f[1], "="); ok && name != baseArg {
				args[name] = strings.Trim(def, `"'`)
			}
			continue
		}
		if !strings.EqualFold(f[0], "FROM") {
			continue
		}
		seenFrom = true
		f = f[1:]
		for len(f) > 0 && strings.HasPrefix(f[0], "--") {
			if strings.HasPrefix(f[0], "--platform") {
				exact = false
			}
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		ref := os.Expand(f[0], func(k string) string {
			if v, ok := args[k]; ok {
				return v
			}
			exact = false
			return "$" + k
		})
		if ref != "" && ref != "scratch" && !strings.Contains(ref, "$") && !stages[strings.ToLower(ref)] {
			refs = append(refs, ref)
		}
		if len(f) >= 3 && strings.EqualFold(f[1], "AS") {
			stages[strings.ToLower(f[2])] = true
		}
	}
	return refs, exact
}

// baseImageIDs resolves each base to the image id the build would use: the
// local one, pulled first when absent, as the build itself would. a base the
// user refreshes with a pull then changes the cache key and forces a
// rebuild, where hashing the tag alone would keep the stale image forever.
func baseImageIDs(ctx context.Context, driver string, refs []string) (string, error) {
	var b strings.Builder
	for _, ref := range refs {
		id, err := exec.CommandContext(ctx, driver, "image", "inspect", "--format", "{{.Id}}", ref).Output()
		if err != nil {
			if out, err := exec.CommandContext(ctx, driver, "pull", "-q", ref).CombinedOutput(); err != nil {
				return "", fmt.Errorf("pull base %s: %w (%s)", ref, err, strings.TrimSpace(string(out)))
			}
			if id, err = exec.CommandContext(ctx, driver, "image", "inspect", "--format", "{{.Id}}", ref).Output(); err != nil {
				return "", fmt.Errorf("inspect base %s: %w", ref, err)
			}
		}
		fmt.Fprintf(&b, "%s=%s\n", ref, strings.TrimSpace(string(id)))
	}
	return b.String(), nil
}

func rehash(hash, extra string) string {
	if extra == "" {
		return hash
	}
	h := sha256.Sum256([]byte(hash + "\n" + extra))
	return hex.EncodeToString(h[:])
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
//
// dfData/dfErr are the dockerfile's contents, already read by the caller (see
// buildImages) -- shared with checkPatsIgnored/usesContext so the dockerfile
// isn't read more than once per build. base is the PATS_SANDBOX build arg,
// "" when the dockerfile doesn't mention it; it joins the hash only when it
// does, so a new pats version rebuilds contexts that derive from the pats
// sandbox and leaves the rest cached.
func buildContextHash(ctxDir, dockerfile string, dfData []byte, dfErr error, base string) (string, error) {
	if dfErr != nil {
		return "", dfErr
	}
	h := sha256.New()
	h.Write(dfData)
	if bytes.Contains(dfData, []byte(baseArg)) {
		fmt.Fprintf(h, "%s=%s\n", baseArg, base)
	}

	var patterns []ignorePattern
	if data, err := os.ReadFile(effectiveIgnoreFile(ctxDir, dockerfile)); err == nil {
		h.Write(data)
		patterns = parseIgnore(string(data))
	}

	if usesContext(dfData, dfErr) {
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
//
// dfData/dfErr are the dockerfile's contents, already read by the caller (see
// buildImages) -- forwarded to usesContext so the dockerfile isn't read again.
func checkPatsIgnored(ctxDir, dockerfile string, dfData []byte, dfErr error) error {
	if _, err := os.Stat(filepath.Join(ctxDir, ".pats")); err != nil {
		return nil // no .pats in the context -- nothing to leak
	}
	if !usesContext(dfData, dfErr) {
		return nil
	}
	ignore := effectiveIgnoreFile(ctxDir, dockerfile)
	if data, err := os.ReadFile(ignore); err == nil && ignoresPats(string(data)) {
		return nil
	}
	return fmt.Errorf("build context %s contains .pats/ (run artifacts) and %s does not exclude it -- add a `.pats/` line", ctxDir, filepath.Base(ignore))
}

// usesContext reports whether a dockerfile reads its build context: any
// COPY/ADD instruction not sourced from another stage (--from=). data/err are
// the dockerfile's contents as already read by the caller (buildImages reads
// it once and shares the result across checkPatsIgnored, buildContextHash,
// and this function). the scan is line-based and errs on the safe side -- a
// read error (err != nil), or a dockerfile with e.g. a heredoc body whose
// lines look like instructions, counts as using the context (worst case the
// .pats guard fires unnecessarily).
func usesContext(data []byte, err error) bool {
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
