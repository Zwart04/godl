package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StartTimeout is how long a caller may wait for a manager it just launched.
//
// A cold start has to load the record store and bind a port, and on a machine
// waking from sleep that is a couple of seconds rather than a few hundred
// milliseconds. Half a minute is generous without letting a broken launch
// hang a browser's messaging host forever.
const StartTimeout = 30 * time.Second

// EnsureRunning brings the manager up if nothing answers, and reports only
// when one is actually reachable.
//
// The browser's native messaging host needs this: a person clicks a download
// link having never opened godl, and there must be a manager to hand it to.
// The alternative - refusing and telling them to start an app they may not
// know is installed - is how a download manager loses its whole reason to
// exist.
func EnsureRunning(timeout time.Duration) error {
	if probeEndpoint() {
		return nil
	}
	if timeout <= 0 {
		timeout = StartTimeout
	}
	exe, err := managerExe()
	if err != nil {
		return err
	}
	if err := startDetached(exe); err != nil {
		return fmt.Errorf("could not start godl: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probeEndpoint() {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("godl was launched but did not come up within " + timeout.Round(time.Second).String())
}

// managerExe locates godl.exe next to whichever binary is running.
//
// The two files ship side by side and nothing else is assumed about where the
// user put them, so a relative lookup from the running executable is the only
// guess that survives being moved to another folder.
func managerExe() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("could not locate godl: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	gui := filepath.Join(filepath.Dir(self), "godl.exe")
	if _, err := os.Stat(gui); err != nil {
		return "", errors.New("godl.exe was not found next to this program")
	}
	return gui, nil
}
