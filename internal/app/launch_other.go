//go:build !windows

package app

import "os/exec"

// startDetached launches godl.exe with no console of its own.
//
// A non-Windows build is only used to compile and test the engine, so the
// plain start is enough there; there is no console object to detach from.
func startDetached(exe string) error {
	return exec.Command(exe).Start()
}
