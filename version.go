// Package pats holds the root-level VERSION embed; go:embed cannot reach
// above a package dir, so the file lives here next to VERSION and
// internal/version reads it from this package.
package pats

import _ "embed"

//go:embed VERSION
var VersionFile string
