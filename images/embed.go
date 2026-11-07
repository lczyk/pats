// Package images holds what pats builds its sandboxes from, for a driver that
// provisions at run time rather than pulling a published image.
package images

import (
	_ "embed"
	"strings"
)

// ProvisionSandbox is provision-sandbox.sh: the pats sandbox's tools, the
// same steps the published docker image is built with.
//
//go:embed provision-sandbox.sh
var ProvisionSandbox string

// HarnessVersions is harness-versions: the harness cli versions this pats
// release pins, read by ProvisionSandbox from beside itself.
//
//go:embed harness-versions
var HarnessVersions string

// pinComponents names the component each pin is for, as the stamps do.
var pinComponents = map[string]string{
	"CLAUDE_CLI_VERSION":  "claude",
	"CODEX_CLI_VERSION":   "codex",
	"COPILOT_CLI_VERSION": "copilot",
	"OPENCODE_VERSION":    "opencode",
}

// ComponentPins is HarnessVersions by component: claude -> 2.1.319.
func ComponentPins() map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(HarnessVersions, "\n") {
		name, ver, ok := strings.Cut(strings.TrimSpace(line), "=")
		if c := pinComponents[name]; ok && c != "" {
			out[c] = ver
		}
	}
	return out
}
