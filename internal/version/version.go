// Package version exposes pats' build-time version info: the human version
// number from the root VERSION file, plus commit/build facts the Go toolchain
// stamps into the binary at build time (see github.com/lczyk/version/go).
package version

import (
	"strings"

	pats "github.com/lczyk/pats"
	ver "github.com/lczyk/version/go"
)

// Info is the resolved build info: VERSION as the fallback version, commit/build
// facts from the toolchain's vcs stamp. print it directly for the full line, or
// read Info.Version for just the number.
var Info = ver.Read(strings.TrimSpace(pats.VersionFile))
