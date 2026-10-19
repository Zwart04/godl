//go:build setup

// Command godl-setup installs godl.
//
// A person should only ever have to deal with one file. godl ships as two
// programs - godl.exe, which they use, and godl-host.exe, which their browser
// launches on their behalf - and asking anyone to place both by hand is how a
// download manager ends up half-installed and quietly broken: the window
// opens, the extension connects, and the very first download does nothing
// because the second file was never copied.
//
// This program carries both programs inside itself, puts them somewhere
// sensible, registers the browser, adds the shortcuts, and gets out of the
// way. It has no wizard, because there is nothing to decide.
//
//	go build -tags setup -H windowsgui -o godl-setup.exe .\cmd\godl-setup
//
// The payload directory must hold gzipped copies of godl.exe and
// godl-host.exe; build-setup.ps1 produces them.
package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"godl/internal/browsersetup"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed payload/godl.exe.zip payload/godl-host.exe.zip
var payload embed.FS

const appName = "godl"

// runKeyName is where Windows looks for programs to start with the session.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func main() {
	dir := flag.String("dir", defaultDir(), "folder to install into")
	uninstall := flag.Bool("uninstall", false, "remove godl instead of installing it")
	noDesktop := flag.Bool("no-desktop", false, "skip the desktop shortcut")
	noStart := flag.Bool("no-autostart", false, "do not start godl with Windows")
	quiet := flag.Bool("quiet", false, "install without showing any dialog")
	flag.Parse()

	var err error
	var said string
	if *uninstall {
		err = remove(*dir)
		said = "godl has been removed from this computer."
	} else {
		if err = install(*dir, !*noDesktop, !*noStart); err == nil {
			said = "godl is installed.\n\n" +
				"It is in " + *dir + " and is running now.\n\n" +
				"Install the browser extension to have downloads handed to it."
		}
	}

	if err != nil {
		say("godl setup", "godl could not finish.\n\n"+err.Error())
		os.Exit(1)
	}
	if !*quiet {
		say("godl setup", said)
	}
}

/* ── install ────────────────────────────────────────────────────────────── */

func install(dir string, desktop, autostart bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}

	// Both programs, and nothing else. The payload is zipped rather than
	// gzipped file by file so the installer's own size stays close to the
	// two executables it carries.
	for _, name := range []string{appName + ".exe", appName + "-host.exe"} {
		if err := unpack(name, filepath.Join(dir, name)); err != nil {
			return err
		}
	}

	host := filepath.Join(dir, appName+"-host.exe")

	// Registration has to happen after the files exist: the registry value
	// points the browser at a manifest that names godl-host.exe by absolute
	// path, and a path to a file that is not there yet is worse than no
	// registration at all.
	if err := register(host); err != nil {
		return err
	}

	if err := shortcut(startMenuShortcut(), filepath.Join(dir, appName+".exe"), dir); err != nil {
		return err
	}
	if desktop {
		if err := shortcut(desktopShortcut(), filepath.Join(dir, appName+".exe"), dir); err != nil {
			return err
		}
	}
	if autostart {
		if err := setAutoStart(filepath.Join(dir, appName+".exe")); err != nil {
			return err
		}
	}

	return launch(filepath.Join(dir, appName+".exe"), dir)
}

// unpack inflates one embedded program and puts it at dest.
//
// The file is written beside its destination and moved into place, so an
// interrupted setup leaves either the old program or the new one and never a
// half-written executable that Windows will refuse to start with a message
// nobody can act on.
func unpack(name, dest string) error {
	zipBytes, err := payload.ReadFile("payload/" + name + ".zip")
	if err != nil {
		return fmt.Errorf("the setup file is missing %s inside itself: %w", name, err)
	}

	body, err := readFirstFile(zipBytes)
	if err != nil {
		return fmt.Errorf("could not read %s from the setup file: %w", name, err)
	}

	staging := dest + ".new"
	out, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("could not write %s: %w", staging, err)
	}
	if _, err := out.Write(body); err != nil {
		out.Close()
		return fmt.Errorf("could not write %s: %w", staging, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("could not write %s: %w", staging, err)
	}

	// A godl that is still running holds its own executable open. Replacing
	// it would fail anyway, so say which program is in the way rather than
	// leaving a .new file behind with no explanation.
	_ = os.Remove(dest)
	if err := os.Rename(staging, dest); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("could not replace %s: %w", dest, err)
	}
	return nil
}

// readFirstFile takes the single entry out of the payload archive.
func readFirstFile(body []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	if len(zr.File) == 0 {
		return nil, fmt.Errorf("the archive is empty")
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// The decompressed size is known from the central directory. Using it
	// means a corrupt entry fails loudly instead of returning short data
	// that produces an executable which does not start.
	want := int64(zr.File[0].UncompressedSize64)
	out := new(bytes.Buffer)
	if _, err := io.Copy(out, io.LimitReader(rc, want+1)); err != nil {
		return nil, err
	}
	if int64(out.Len()) != want {
		return nil, fmt.Errorf("expected %d bytes, got %d", want, out.Len())
	}
	return out.Bytes(), nil
}

/* ── uninstall ──────────────────────────────────────────────────────────── */

func remove(dir string) error {
	// The browser must stop looking for the host before the host disappears.
	// Doing it the other way round leaves the browser launching a file that
	// is not there, which it reports as a broken extension.
	_ = unregister()

	for _, path := range []string{startMenuShortcut(), desktopShortcut()} {
		_ = os.Remove(path)
	}
	_ = clearAutoStart()

	// A folder that still holds a running godl.exe cannot be deleted, and
	// deleting it is not worth killing a transfer for. Say so and leave it.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("godl was uninstalled but %s could not be removed "+
			"(it may still be running): %w", dir, err)
	}

	// Programs, not godl's own data. Everything in %APPDATA%\godl - the
	// record of every download - survives an uninstall unless the person
	// deletes it themselves.
	_ = os.Remove(filepath.Dir(dir))
	return nil
}

/* ── registration ───────────────────────────────────────────────────────── */

// The installer writes the very same registry keys and manifests that godl.exe
// rewrites on every launch, because it calls the same package. Two
// implementations of this table is how one of them eventually points the
// browser at a file the other one moved.
func register(host string) error { return browsersetup.Register(host) }

func unregister() error { return browsersetup.Unregister() }

/* ── shortcuts and startup ──────────────────────────────────────────────── */

func startMenuShortcut() string {
	return filepath.Join(os.Getenv("APPDATA"),
		`Microsoft\Windows\Start Menu\Programs`, appName+".lnk")
}

func desktopShortcut() string {
	return filepath.Join(os.Getenv("USERPROFILE"), "Desktop", appName+".lnk")
}

// shortcut asks Windows' own shell link object to make the .lnk.
//
// Writing the lnk binary format by hand is possible and would remove the
// dependency, but it is two hundred lines of a documented-but-fiddly format
// that only ever breaks cosmetically. The shell is the authority on it.
func shortcut(path, target, workDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("could not create the shortcut folder: %w", err)
	}
	script := fmt.Sprintf(
		"$s = (New-Object -ComObject WScript.Shell).CreateShortcut('%s'); "+
			"$s.TargetPath = '%s'; "+
			"$s.WorkingDirectory = '%s'; "+
			"$s.Description = 'Download manager'; "+
			"$s.IconLocation = '%s,0'; "+
			"$s.Save()",
		psQuote(path), psQuote(target), psQuote(workDir), psQuote(target))

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not create the shortcut: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

func setAutoStart(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("could not open the startup list: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(appName, `"`+exe+`"`); err != nil {
		return fmt.Errorf("could not add godl to the startup list: %w", err)
	}
	return nil
}

func clearAutoStart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	_ = k.DeleteValue(appName)
	return nil
}

/* ── launching ──────────────────────────────────────────────────────────── */

const detachedProcess = 0x00000008

// launch starts the freshly installed program without tying it to this one.
//
// Closing the setup window must not close godl, and a setup started from a
// terminal must not leave godl holding that terminal's console.
func launch(exe, workDir string) error {
	cmd := exec.Command(exe)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("godl was installed but could not be started: %w", err)
	}
	// Deliberately not waited for: godl is meant to outlive its installer.
	go func() { _ = cmd.Wait() }()
	return nil
}

/* ── small helpers ──────────────────────────────────────────────────────── */

func defaultDir() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "Programs", appName)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "AppData", "Local", "Programs", appName)
	}
	return filepath.Join(os.TempDir(), appName)
}

// say shows a message box. A setup program built as a windowed application
// has no console, so this is the only voice it has.
func say(caption, text string) {
	c, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return
	}
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONINFORMATION)
}
