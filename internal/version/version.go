// Package version exposes pats' build-time version info: the human version
// number from VERSION, plus commit/build facts the Go toolchain stamps into
// the binary at build time (see github.com/lczyk/version/go).
package version

import (
	_ "embed"
	"strings"

	ver "github.com/lczyk/version/go"
)

//go:embed VERSION
var versionFile string

var info = ver.Read(strings.TrimSpace(versionFile))

var (
	Version   = info.Version
	CommitSHA = info.CommitSHA
	BuildDate = info.BuildDate
	BuildInfo = info.BuildInfo
)
