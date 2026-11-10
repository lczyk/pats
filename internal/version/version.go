// Package version exposes pats' build-time version info: the human version
// number from the root VERSION file, plus commit/build facts the Go toolchain
// stamps into the binary at build time (see github.com/lczyk/version/go).
package version

import (
	"runtime/debug"
	"strings"

	pats "github.com/lczyk/pats"
	ver "github.com/lczyk/version/go"
)

// Info is the resolved build info: VERSION as the fallback version, commit/build
// facts from the toolchain's vcs stamp. print it directly for the full line, or
// read Info.Version for just the number.
var Info = ver.Read(strings.TrimSpace(pats.VersionFile))

// Tags are the build tags the binary was built with ("" for none), as the
// toolchain recorded them: a build that leaves drivers out names them here.
func Tags() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "-tags" {
			return s.Value
		}
	}
	return ""
}
