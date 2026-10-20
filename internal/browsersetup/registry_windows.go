package browsersetup

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Chrome and Edge keep separate tables that read the same manifest, so one
// manifest path is written to three keys.
const (
	chromeKey = `Software\Google\Chrome\NativeMessagingHosts\` + ChromeHostName
	edgeKey   = `Software\Microsoft\Edge\NativeMessagingHosts\` + ChromeHostName
	mozKey    = `Software\Mozilla\NativeMessagingHosts\` + FirefoxHostName
)

func writeKeys(chromeManifest, firefoxManifest string) error {
	var errs []error
	for _, pair := range [][2]string{
		{chromeKey, chromeManifest},
		{edgeKey, chromeManifest},
		{mozKey, firefoxManifest},
	} {
		if err := setDefault(pair[0], pair[1]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeKeys() []error {
	var errs []error
	for _, key := range []string{chromeKey, edgeKey, mozKey} {
		if err := deleteTree(registry.CURRENT_USER, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func keysPresent() bool {
	for _, key := range []string{chromeKey, edgeKey, mozKey} {
		if _, err := readDefault(key); err != nil {
			return false
		}
	}
	return true
}

// keysTargets says where each browser table points right now, by key path.
//
// These keys are the machine's, not the process's: what they name is somebody's
// working browser hook. Anything that has to rewrite them - a test above, a
// future installer - needs to put back the exact value it found rather than a
// path of its own, because "where my own godl-host.exe happens to be" is not
// the same question as "where this was pointing".
func keysTargets() map[string]string {
	out := make(map[string]string, 3)
	for _, key := range []string{chromeKey, edgeKey, mozKey} {
		if value, err := readDefault(key); err == nil {
			out[key] = value
		}
	}
	return out
}

// restoreKeys writes a snapshot taken by keysTargets back, and removes any key
// the snapshot did not have. That second half is what makes a machine that was
// not registered before restorable too: it goes back to being unregistered
// rather than half-hooked to whatever the run left behind.
func restoreKeys(snapshot map[string]string) error {
	var errs []error
	for _, key := range []string{chromeKey, edgeKey, mozKey} {
		if value, ok := snapshot[key]; ok {
			errs = append(errs, setDefault(key, value))
			continue
		}
		errs = append(errs, deleteTree(registry.CURRENT_USER, key))
	}
	return errors.Join(errs...)
}

// setDefault creates the key if needed and makes its default value the
// manifest path. A key left over from an older build keeps any other values
// it had, which is why only the default value is touched.
func setDefault(keyPath, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open %s: %w", keyPath, err)
	}
	defer k.Close()
	if err := k.SetStringValue("", value); err != nil {
		return fmt.Errorf("set %s: %w", keyPath, err)
	}
	return nil
}

func readDefault(keyPath string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	return v, err
}

// deleteTree removes one key, tolerating its absence.
//
// Windows refuses DeleteKey on a key that still has children. godl only ever
// writes one value, so a child means something else owns the key - leave it
// alone rather than risk removing another application's registration.
func deleteTree(root registry.Key, path string) error {
	err := registry.DeleteKey(root, path)
	if err == nil || errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if strings.Contains(err.Error(), "cannot delete") {
		return nil
	}
	return fmt.Errorf("remove %s: %w", path, err)
}
