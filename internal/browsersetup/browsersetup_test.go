package browsersetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"godl/internal/config"
)

// The browser parses these manifests with a strict reader, and a path is the
// one field where a mistake is invisible until someone clicks a link. Windows
// paths carry backslashes, which JSON must escape; getting that wrong produces
// a file the browser silently refuses to load.
func TestManifestsCarryAValidPathAndTheRightTrust(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the manifest layout is only meaningful for the browsers godl ships for")
	}
	t.Setenv(config.EnvHome, t.TempDir())
	// Before the first write. This is the only moment the machine is still
	// exactly as it was found, so a snapshot taken later would faithfully
	// restore the test's own values.
	restoreRegistration(t)

	host := `C:\Users\Some One\godl\godl-host.exe`
	if err := Register(host); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	chrome := readManifest(t, ChromeHostName)
	if chrome.Path != host {
		t.Errorf("chrome path = %q, want %q", chrome.Path, host)
	}
	if chrome.Type != "stdio" {
		t.Errorf("chrome type = %q, want stdio", chrome.Type)
	}
	wantOrigin := "chrome-extension://" + ChromeExtensionID + "/"
	if len(chrome.AllowedOrigins) != 1 || chrome.AllowedOrigins[0] != wantOrigin {
		t.Errorf("chrome allowed_origins = %v, want exactly [%q]", chrome.AllowedOrigins, wantOrigin)
	}
	if len(chrome.AllowedExtensions) != 0 {
		t.Errorf("chrome manifest carries allowed_extensions = %v, Chrome does not read them",
			chrome.AllowedExtensions)
	}

	firefox := readManifest(t, FirefoxHostName)
	if firefox.Path != host {
		t.Errorf("firefox path = %q, want %q", firefox.Path, host)
	}
	if len(firefox.AllowedExtensions) != 1 || firefox.AllowedExtensions[0] != FirefoxExtensionID {
		t.Errorf("firefox allowed_extensions = %v, want exactly [%q]",
			firefox.AllowedExtensions, FirefoxExtensionID)
	}
	if len(firefox.AllowedOrigins) != 0 {
		t.Errorf("firefox manifest carries allowed_origins = %v, Firefox does not read them",
			firefox.AllowedOrigins)
	}
}

func TestRegisterRefusesAnEmptyHostPath(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	if err := Register(""); err == nil {
		t.Fatal("Register(\"\") succeeded, want a refusal")
	}
}

func TestUnregisterLeavesNothingBehind(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("registry keys only exist on Windows")
	}
	t.Setenv(config.EnvHome, t.TempDir())
	restoreRegistration(t)

	if err := Register(`C:\godl\godl-host.exe`); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if !Registered() {
		t.Fatal("Registered() = false right after Register(), want true")
	}
	if err := Unregister(); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if Registered() {
		t.Fatal("Registered() = true after Unregister(), want false")
	}
	for _, name := range []string{ChromeHostName, FirefoxHostName} {
		if _, err := os.Stat(filepath.Join(config.Dir(), name+".json")); !os.IsNotExist(err) {
			t.Errorf("%s.json still present after Unregister()", name)
		}
	}
}

// Unregistering twice is what a user does when they try to be sure. It must
// not turn a completed cleanup into an error report.
func TestUnregisterIsIdempotent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("registry keys only exist on Windows")
	}
	t.Setenv(config.EnvHome, t.TempDir())

	restoreRegistration(t)
	if err := Unregister(); err != nil {
		t.Fatalf("Unregister() on a clean directory error = %v", err)
	}
	if err := Unregister(); err != nil {
		t.Fatalf("second Unregister() error = %v, want it to stay quiet", err)
	}
}

// restoreRegistration puts the machine back the way the test found it.
//
// These tests touch the real per-user registry, and someone running them may
// have a working godl registration they would not want a test run to remove.
//
// The values are read back rather than recomputed. Asking HostExePath() for a
// path to re-register sounds equivalent and is not: under `go test` that is a
// directory built moments ago and thrown away afterwards, so the browser is
// left pointed at a manifest that no longer exists - or, if that lookup fails,
// left unhooked altogether. Which is how a developer's extension goes quiet
// after an innocent `go test`, and why the snapshot has to be of the machine
// and not of the test binary.
func restoreRegistration(t *testing.T) {
	t.Helper()
	before := keysTargets()
	t.Cleanup(func() {
		_ = Unregister()
		if err := restoreKeys(before); err != nil {
			t.Errorf("putting the browser registration back: %v", err)
		}
	})
}

type rawManifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

func readManifest(t *testing.T, name string) rawManifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(config.Dir(), name+".json"))
	if err != nil {
		t.Fatalf("read %s manifest: %v", name, err)
	}
	var m rawManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s manifest is not valid JSON: %v\n%s", name, err, body)
	}
	if m.Name != name {
		t.Errorf("manifest name = %q, want %q", m.Name, name)
	}
	if strings.Contains(m.Path, "/") {
		t.Errorf("manifest path = %q, want Windows backslashes only", m.Path)
	}
	return m
}
