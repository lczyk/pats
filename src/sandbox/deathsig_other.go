//go:build !linux

package sandbox

import "syscall"

// dieWithParent is linux's alone: elsewhere a child outlives a killed pats.
func dieWithParent(*syscall.SysProcAttr) {}
