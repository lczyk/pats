//go:build !linux && !darwin

package sandbox

// bootID: no way to read one here, so no identity and no sweep.
func bootID() string { return "" }

func pidNS() string { return "" }
