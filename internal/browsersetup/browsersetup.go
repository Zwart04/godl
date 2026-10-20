// Package browsersetup connects godl to the browser's native messaging table.
//
// A browser extension cannot reach a download manager on its own. It asks the
// browser to launch a small helper executable - the native messaging host -
// and the browser will only do that for hosts listed in a per-user registry
// key that names a manifest file on disk. This package owns that wiring: the
// two manifests, the four browser tables, and the ability to undo all of it.
//
// Nothing here talks to the browser or to godl itself; it only records where
// godl-host.exe lives so the browser has an address to knock on.
package browsersetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"godl/internal/config"
)

// The two host names are deliberately different files. Chrome and Edge read
// one manifest shape, Firefox reads another (allowed_origins versus
// allowed_extensions), so sharing a manifest would mean one of them rejects it.
const (
	ChromeHostName  = "com.godl.host"
	FirefoxHostName = "com.godl.moz"
)

// Extension IDs must be stable, because the browser pins a native messaging
// host to the extension that is allowed to talk to it. Chrome derives the ID
// from the manifest's pinned public key, and Firefox reads it from
// browser_specific_settings.gecko.id - both are generated for godl, never
// borrowed from another extension, so the trust chain is ours end to end.
const (
	ChromeExtensionID   = "afamnbklhodbngmbljglpkklkidfjccn"
	FirefoxExtensionID  = "godl@davdigi.com"
	ChromeExtensionDesc = "godl download bridge"
)

type manifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// HostExePath returns the absolute path of godl-host.exe.
//
// The browser launches this file directly, so a relative path or a path with
// a forward slash would be a silent failure - it is resolved and cleaned
// here rather than trusted to the caller.
func HostExePath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate godl: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		// A missing symlink target is not worth failing over; the raw path
		// is still usable.
		self, _ = os.Executable()
	}
	host := filepath.Join(filepath.Dir(self), "godl-host.exe")
	if _, err := os.Stat(host); err != nil {
		return "", fmt.Errorf("godl-host.exe was not found next to godl.exe")
	}
	return filepath.Clean(host), nil
}

// Register writes the manifests and the browser registry keys.
//
// It is idempotent: running it after an upgrade simply rewrites the paths, so
// it is safe to call on every launch and after moving the folder.
func Register(hostExe string) error {
	if hostExe == "" {
		return errors.New("no godl-host.exe path was given")
	}
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config folder: %w", err)
	}

	chrome := manifest{
		Name:           ChromeHostName,
		Description:    ChromeExtensionDesc,
		Path:           hostExe,
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://" + ChromeExtensionID + "/"},
	}
	firefox := manifest{
		Name:              FirefoxHostName,
		Description:       ChromeExtensionDesc,
		Path:              hostExe,
		Type:              "stdio",
		AllowedExtensions: []string{FirefoxExtensionID},
	}

	chromePath, err := writeManifest(dir, ChromeHostName, chrome)
	if err != nil {
		return err
	}
	firefoxPath, err := writeManifest(dir, FirefoxHostName, firefox)
	if err != nil {
		return err
	}

	if err := writeKeys(chromePath, firefoxPath); err != nil {
		return err
	}
	return nil
}

// Unregister removes every key and manifest this package created.
//
// A key that is already gone is not an error: the point of uninstalling is
// that nothing is left, and that is exactly what "not found" means.
func Unregister() error {
	dir := config.Dir()
	var errs []error
	for _, name := range []string{ChromeHostName, FirefoxHostName} {
		if err := os.Remove(filepath.Join(dir, name+".json")); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, removeKeys()...)
	return errors.Join(errs...)
}

// Registered reports whether the browser table currently points at a host.
//
// It checks the keys rather than the manifests, because the browser trusts
// the registry; a manifest left behind by an interrupted upgrade cannot make
// a download appear in the browser.
func Registered() bool {
	return keysPresent()
}

func writeManifest(dir, name string, m manifest) (string, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode %s manifest: %w", name, err)
	}
	full := filepath.Join(dir, name+".json")
	if err := os.WriteFile(full, append(body, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s manifest: %w", name, err)
	}
	return full, nil
}
