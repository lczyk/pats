package sandbox

import "golang.org/x/sys/unix"

// bootID is the kernel's random id for this boot.
func bootID() string {
	s, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return s
}

// pidNS is "-": macos has no pid namespaces.
func pidNS() string { return "-" }
