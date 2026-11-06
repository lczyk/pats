package sandbox

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/pats/internal/version"
)

// TestImagePinsEveryVersionShape covers every version string Image may be
// handed: the bare semver version.Read produces (a tag, with any pre-release
// suffix), plus the raw toolchain shapes (v-prefixed, dirty, pseudo-version,
// devel) in case a caller sets the version by hand.
func TestImagePinsEveryVersionShape(t *testing.T) {
	saved := version.Info
	t.Cleanup(func() { version.Info = saved })

	// made-up versions, so a release bump of the real one never rewrites them.
	const latest = proxyRepo + ":latest"
	for _, tc := range []struct {
		name    string
		version string
		image   string
		warns   bool
	}{
		{"module stamp", "v1.2.3", proxyRepo + ":v1.2.3", false},
		{"version file fallback", "1.2.3", proxyRepo + ":v1.2.3", false},
		{"beta", "v1.0.0-beta.1", proxyRepo + ":v1.0.0-beta.1", false},
		{"rc", "1.0.0-rc", proxyRepo + ":v1.0.0-rc", false},
		{"dotless rc", "v1.0.0-rc1", proxyRepo + ":v1.0.0-rc1", false},
		{"other suffix", "v1.0.0-next", latest, true},
		{"dirty", "v1.2.3-dirty", latest, true},
		{"pseudo-version", "v1.0.0-beta.1.0.20260101120000-abcdef123456", latest, true},
		{"branch pseudo-version", "v0.0.0-20260101120000-abcdef123456", latest, true},
		{"devel", "(devel)", latest, true},
		{"unset", "", latest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.Info.Version = tc.version
			image, warn := ProxyImage("")
			assert.Equal(t, image, tc.image)
			assert.Equal(t, warn != "", tc.warns)
			if tc.warns {
				// the warning quotes what the binary reports, not the trimmed form
				assert.That(t, strings.Contains(warn, fmt.Sprintf("%q", tc.version)))
			}
		})
	}
}

// the sandbox default follows the same rule against its own repo.
func TestSandboxImage(t *testing.T) {
	saved := version.Info
	t.Cleanup(func() { version.Info = saved })

	version.Info.Version = "v1.2.3"
	image, warn := SandboxImage("")
	assert.Equal(t, image, sandboxRepo+":v1.2.3")
	assert.Equal(t, warn, "")

	version.Info.Version = ""
	image, warn = SandboxImage("")
	assert.Equal(t, image, sandboxRepo+":latest")
	assert.ContainsString(t, warn, "image:")
}

// TestImageConfiguredWins keeps the override ahead of any version logic.
func TestImageConfiguredWins(t *testing.T) {
	saved := version.Info
	t.Cleanup(func() { version.Info = saved })
	version.Info.Version = ""

	image, warn := ProxyImage("my/own:build")
	assert.Equal(t, image, "my/own:build")
	assert.Equal(t, warn, "")
	image, warn = SandboxImage("my/own:build")
	assert.Equal(t, image, "my/own:build")
	assert.Equal(t, warn, "")
}
