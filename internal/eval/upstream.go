package eval

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// upstreamPrefix marks a pin's input that is an upstream image: the ref
// after it, the digest (container) or fingerprint (lxd) it had at init.
const upstreamPrefix = "upstream:"

// upstreamBudget is all the upstream checks of a run get, together.
const upstreamBudget = 5 * time.Second

// the lookups, vars so tests fake them.
var (
	registryDigest       = sandbox.RegistryDigest
	lxdRemoteFingerprint = sandbox.LXDRemoteFingerprint
)

// publishedRepo says whether ref is one of pats's own published images at
// a release's tag (v1.0.0-beta.5), which never changes upstream. its
// :latest or ubuntu tags do, and are checked like any image.
func publishedRepo(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "ghcr.io/lczyk/pats/")
	if !ok {
		return false
	}
	_, tag, ok := strings.Cut(rest, ":")
	return ok && strings.HasPrefix(tag, "v") && sandbox.IsRelease(tag)
}

// upstreamInputs records, at init, what a sandbox's image came from
// upstream: an image: sandbox's own ref, and the FROM images of build: and
// base: (container), or the base on an image remote (lxd).
func upstreamInputs(ctx context.Context, sb config.Sandbox, configDir string, h imageHost, e *pinEntry) {
	add := func(ref, id string) {
		if ref != "" && id != "" && !publishedRepo(ref) {
			e.Inputs[upstreamPrefix+ref] = id
		}
	}
	if sb.Driver == "lxd" {
		// a base the build keyed on is recorded by initLXD; an image of
		// another remote was copied, and its fingerprint is the pin's.
		if strings.Contains(sb.Image, ":") {
			add(sb.Image, e.Image.ID)
		}
		return
	}
	// a built image's FROM images were recorded before its build (see
	// baseDigests); an image: sandbox's, or the pats sandbox's (:latest on a
	// dev build), is the pinned image itself.
	if !provisions(sb) && sb.Build == "" {
		add(e.Image.Name, e.Image.Digest)
	}
}

// fromImages are the images a container sandbox's build starts FROM.
func fromImages(sb config.Sandbox, configDir string) []string {
	published, _ := sandbox.SandboxImage("")
	switch {
	case provisions(sb) && sb.Base != "":
		return []string{sb.Base}
	case provisions(sb):
		return []string{published} // a layer on the pats sandbox
	case sb.Build != "":
		if _, dockerfile, err := resolveBuildSpec(configDir, sb.Build); err == nil {
			refs, _ := sandbox.DockerfileFroms(dockerfile, published)
			return refs
		}
	}
	return nil
}

// baseDigests pins the digests of a build's FROM images before it runs,
// pulling an absent one as the build would: so the baseline is the base the
// build used, and one buildkit would not keep locally is still recorded.
// a base that cannot be pulled or has no registry digest is left out.
func baseDigests(ctx context.Context, sb config.Sandbox, configDir string, h puller, log io.Writer) map[string]string {
	out := map[string]string{}
	for _, ref := range fromImages(sb, configDir) {
		if !h.present(ctx, ref) {
			if err := h.pull(ctx, ref, log); err != nil {
				continue
			}
		}
		if _, digest, err := h.inspect(ctx, ref); err == nil && digest != "" {
			out[ref] = digest
		}
	}
	return out
}

// upstreamCheck is one sandbox's upstream refs, for a run to compare.
type upstreamCheck struct {
	sb     string
	lxd    bool
	inputs map[string]string
}

// checkUpstream looks up every check's refs at once, within the budget, and
// returns a warning for each that moved on; a lookup that fails or runs out
// of time is skipped, as is everything when ctx ends.
func checkUpstream(ctx context.Context, checks []upstreamCheck, budget time.Duration) []string {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// read once: a lookup past the budget runs on after this returns.
	registry, lxd := registryDigest, lxdRemoteFingerprint
	var mu sync.Mutex
	var warns []string
	var wg sync.WaitGroup
	for _, c := range checks {
		for k, pinned := range c.inputs {
			ref, ok := strings.CutPrefix(k, upstreamPrefix)
			if !ok {
				continue
			}
			wg.Add(1)
			go func(c upstreamCheck, ref, pinned string) {
				defer wg.Done()
				var now string
				var err error
				if c.lxd {
					now, err = lxd(ctx, ref)
				} else {
					now, err = registry(ctx, ref)
				}
				if err != nil || ctx.Err() != nil || now == pinned {
					return
				}
				mu.Lock()
				warns = append(warns, fmt.Sprintf("sandbox %q: a newer %s is upstream (pinned %s, now %s); pats init --sandbox %s to rebuild on it",
					c.sb, ref, short(pinned), short(now), c.sb))
				mu.Unlock()
			}(c, ref, pinned)
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	out := append([]string(nil), warns...)
	sort.Strings(out)
	return out
}
