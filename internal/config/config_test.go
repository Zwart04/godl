package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirFollowsTheHomeOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)

	if got := Dir(); got != home {
		t.Fatalf("Dir() = %q, want the override %q", got, home)
	}
}

// A download nobody can find is, from where they are sitting, a download that
// did not happen. That is what this test is really about: the payload belongs
// in the person's own Downloads folder, not in a profile directory that takes
// three clicks and a knowing eye to reach.
func TestFilesDirLivesUnderTheDownloadsFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, "")
	t.Setenv("USERPROFILE", home)

	want := filepath.Join(home, "Downloads", "godl")
	if got := FilesDir(); got != want {
		t.Fatalf("FilesDir() = %q, want %q so a finished file is findable", got, want)
	}
}

// The override is for tests, portable copies and throwaway profiles. One of
// those dropping a file into the real Downloads folder is the same class of
// surprise as it rewriting the machine's browser table.
func TestFilesDirStaysInsideAScratchHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)

	want := filepath.Join(home, "files")
	if got := FilesDir(); got != want {
		t.Fatalf("FilesDir() = %q, want %q so a scratch run leaves the real Downloads alone", got, want)
	}
}

// Downloads can be unwritable - redirected by policy, on a drive that just
// filled up, or a file sitting where the folder should be. Failing to start
// the download at all would be the worst of the three answers.
func TestEnsureFilesDirFallsBackWhenDownloadsRefuses(t *testing.T) {
	home := t.TempDir()

	// A file where the Downloads directory belongs. MkdirAll cannot work
	// around it, which is exactly the shape of the real failure.
	if err := os.WriteFile(filepath.Join(home, "Downloads"), []byte("not a folder"), 0o600); err != nil {
		t.Fatalf("set up the blocked Downloads path: %v", err)
	}

	appData := filepath.Join(home, "AppData")
	t.Setenv("APPDATA", appData)
	t.Setenv("USERPROFILE", home)
	t.Setenv(EnvHome, "")

	got, err := EnsureFilesDir()
	if err != nil {
		t.Fatalf("EnsureFilesDir() = %v, want the profile fallback rather than an error", err)
	}

	want := filepath.Join(appData, "godl", "files")
	if got != want {
		t.Fatalf("EnsureFilesDir() = %q, want the fallback %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the fallback directory was reported but not created: %v", err)
	}
}

func TestEnsureFilesDirCreatesWhatItReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)

	got, err := EnsureFilesDir()
	if err != nil {
		t.Fatalf("EnsureFilesDir() = %v", err)
	}
	if !strings.HasPrefix(got, home) {
		t.Fatalf("EnsureFilesDir() = %q, want it inside the scratch home %q", got, home)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the directory was reported but not created: %v", err)
	}
}
