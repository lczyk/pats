// Package images holds what pats builds its sandboxes from, for a driver that
// provisions at run time rather than pulling a published image.
package images

import _ "embed"

// ProvisionSandbox is provision-sandbox.sh: the pats sandbox's tools, the
// same steps the published docker image is built with.
//
//go:embed provision-sandbox.sh
var ProvisionSandbox string
