package sandbox

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lczyk/pats/internal/version"
)

// pats publishes two images per release, both tagged v<version> by
// publish_images.yml: the sandbox (harness clis on ubuntu) a config falls back
// to when it names no image or build context, and the egress proxy (the
// sibling src/sandbox/proxy package -- the two speak the PROXY_* env
// protocol). :latest follows the newest stable release.
const (
	sandboxRepo = "ghcr.io/lczyk/pats/sandbox"
	proxyRepo   = "ghcr.io/lczyk/pats/egress-proxy"
)

// SandboxImage resolves the default sandbox image; see Image.
func SandboxImage(configured string) (image, warn string) {
	return Image(sandboxRepo, configured, "set image: to pin")
}

// ProxyImage resolves a sandbox's egress proxy image; see Image.
func ProxyImage(configured string) (image, warn string) {
	return Image(proxyRepo, configured, "set a sandbox proxy-image to pin")
}

// Image resolves one of pats' published images and, when it can't pin to a
// version, a warning to surface. a configured ref wins. otherwise the default
// is repo:v<version> for this build's pats version -- the proxy's filtering
// protocol and the sandbox's cli set are coupled to the binary, so :latest
// could be a mismatched newer build. if the version isn't a release (dev,
// dirty, unset -> no image was published for it), fall back to :latest and
// return a warning naming the hint; the caller logs it once.
//
// version.Read hands over a bare semver (the module tag, else the VERSION
// file), but a leading v is stripped anyway so a caller setting the version
// by hand can't make release installs -- the ones that most need a matching
// image -- the only builds that never pin.
func Image(repo, configured, hint string) (image, warn string) {
	if configured != "" {
		return configured, ""
	}
	v := strings.TrimPrefix(version.Info.Version, "v")
	if !releaseVersion(v) {
		return repo + ":latest", fmt.Sprintf("pats version %q is not a release; using %s:latest, which may not match this build (%s)", version.Info.Version, repo, hint)
	}
	return repo + ":v" + v, ""
}

// releaseBuild says whether this binary is a tagged install of a release:
// no vcs stamp, a release-shaped version. a clone build counts as dev even
// at its tag.
func releaseBuild() bool {
	return version.Info.CommitSHA == "" && IsRelease(version.Info.Version)
}

// IsRelease reports whether v (a leading v allowed) is a tagged release
// version, one with published images.
func IsRelease(v string) bool { return releaseVersion(strings.TrimPrefix(v, "v")) }

// releaseVersion reports whether v is a tagged release: X.Y.Z, optionally
// with an alpha / beta / rc pre-release (1.0.0-beta.1, 1.0.0-rc1) -- the shapes that
// have a published v<version> image. anything else (empty, a dev or dirty
// suffix, a pseudo-version) has none, so the caller falls back to :latest.
// callers pass the version with any leading v already stripped.
func releaseVersion(v string) bool {
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !digits(p) {
			return false
		}
	}
	return pre == "" || preRelease.MatchString(pre)
}

var preRelease = regexp.MustCompile(`^(alpha|beta|rc)\.?[0-9]*$`)

func digits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
