//go:build !windows

package browsersetup

import "errors"

// Browser registration is a Windows-only concern here: native messaging hosts
// are addressed by the Windows registry on every browser godl supports.

func writeKeys(chromeManifest, firefoxManifest string) error {
	return errors.New("browser registration is only supported on Windows")
}

func removeKeys() []error { return nil }

func keysPresent() bool { return false }

// There are no browser tables to read here, so a snapshot is empty - and an
// empty snapshot restores to nothing, which is exactly the state of the
// machine.
func keysTargets() map[string]string { return nil }

func restoreKeys(map[string]string) error { return nil }
