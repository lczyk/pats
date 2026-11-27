//go:build !linux

package nestedprobe

import "errors"

var errNotLinux = errors.New("the probe runs on linux only")

func checkKVM() (string, error)        { return Missing, errNotLinux }
func checkVhostVsock() (string, error) { return Missing, errNotLinux }
func checkVsock() (string, error)      { return Missing, errNotLinux }
func checkVhostNet() (string, error)   { return Missing, errNotLinux }
