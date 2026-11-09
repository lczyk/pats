package sandbox

import "syscall"

// dieWithParent has the kernel end the child when pats dies, so a killed
// pats leaves no proxy behind.
func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
