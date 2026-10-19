package app

import (
	"os/exec"
	"syscall"
)

// detachedProcess is the Win32 CREATE_PROCESS flag of the same name. The
// syscall package does not export it, and inventing a name here would be
// worse than copying the documented value.
const detachedProcess = 0x00000008

// startDetached launches godl.exe in its own console-less process.
//
// DETACHED_PROCESS is what stops the child from inheriting the parent's
// console. When Chrome starts godl-host there is none to inherit, but the
// same call is correct for a host started by hand from a terminal - without
// it, closing that terminal would take the download manager with it.
func startDetached(exe string) error {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess,
	}
	return cmd.Start()
}
