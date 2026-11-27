package sandbox

import (
	"os"
	"strings"
)

// bootID is the kernel's random id for this boot.
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pidNS is this process's pid namespace, as its link names it (pid:[n]).
func pidNS() string {
	l, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return l
}
