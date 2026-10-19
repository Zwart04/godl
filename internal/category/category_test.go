package category

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssignSortsByExtensionAlone(t *testing.T) {
	list := Defaults()

	for _, tc := range []struct{ file, want string }{
		{"SETUP.EXE", "Programs"}, // extensions are matched without case
		{"installer.msi", "Programs"},
		{"ubuntu.iso", "Programs"},
		{"backup.7z", "Archives"},
		{"report.pdf", "Documents"},
		{"song.FLAC", "Music"},
		{"clip.mkv", "Video"},
		{"photo.heic", "Pictures"},
		{"notes", "Other"},             // no extension at all
		{"archive.tar.gz", "Archives"}, // the last extension is the one that counts
		{"something.unknownext", "Other"},
		{".gitignore", "Other"}, // a leading dot is read as the extension, and matches nothing
	} {
		if got := Assign(list, tc.file); got != tc.want {
			t.Errorf("Assign(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestEveryCategoryHasAFolderAndTheCatchAllIsLast(t *testing.T) {
	list := Defaults()

	if got := list[len(list)-1].Name; got != OtherName {
		t.Fatalf("last category = %q, want %q so unmatched files have somewhere to land", got, OtherName)
	}
	for _, c := range list {
		if strings.TrimSpace(c.Name) == "" {
			t.Error("a category has no name")
		}
		if SanitizeFolder(c.Folder) != c.Folder || c.Folder == "" {
			t.Errorf("%q has an unsafe folder %q", c.Name, c.Folder)
		}
	}
}

func TestLoadFallsBackWhenThereIsNoFile(t *testing.T) {
	list := Load(t.TempDir())
	if len(list) != len(Defaults()) {
		t.Fatalf("got %d categories, want the %d built in", len(list), len(Defaults()))
	}
}

// A settings file that cannot be read must not stop downloads landing.
func TestLoadFallsBackWhenTheFileIsUnusable(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(Load(dir)); got != len(Defaults()) {
		t.Fatalf("a malformed file gave %d categories, want the %d built in", got, len(Defaults()))
	}

	// The right shape but not the right type is just as unusable.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"name":"x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(Load(dir)); got != len(Defaults()) {
		t.Fatalf("a file of the wrong shape gave %d categories, want the %d built in", got, len(Defaults()))
	}
}

func TestLoadFoldsTheUsersEntriesOverTheBuiltIns(t *testing.T) {
	dir := t.TempDir()
	// Only two entries: the rest must still be there, because nobody should
	// have to copy out the whole default list to add one folder of their own.
	const file = `[
		{"name":"ISOs","folder":"Disc Images","extensions":["iso","img"]},
		{"name":"Music","folder":"Audio","extensions":["mp3"]}
	]`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(file), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	list := Load(dir)

	if got := Assign(list, "ubuntu.iso"); got != "ISOs" {
		t.Fatalf("iso sorted to %q, want the new ISOs category", got)
	}
	if got := FolderFor(list, "ISOs"); got != "Disc Images" {
		t.Fatalf("ISOs folder = %q, want %q", got, "Disc Images")
	}
	// The built-in was replaced rather than shadowed by a second Music.
	if got := FolderFor(list, "Music"); got != "Audio" {
		t.Fatalf("Music folder = %q, want the override %q", got, "Audio")
	}
	// Everything the file did not mention is untouched.
	if got := Assign(list, "report.pdf"); got != "Documents" {
		t.Fatalf("pdf sorted to %q, want Documents", got)
	}

	if got := list[len(list)-1].Name; got != OtherName {
		t.Fatalf("last category = %q, want the catch-all to stay last", got)
	}
}

// A folder name is joined to a path the user chose, so anything in it that
// looks like a path has to go.
func TestSanitizeFolderKeepsNamesInsideTheDownloadDirectory(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"Programs", "Programs"},
		{"  Music  ", "Music"},
		{`..\..\Windows`, "Windows"},
		{`C:\Windows`, "C Windows"},
		{"..", ""},
		{".", ""},
		// Windows trims trailing dots and spaces from a path component before
		// opening it, so these have to come back empty rather than as a name
		// the filesystem would read back as "..".
		{".. ", ""},
		{"...", ""},
		{". .", ""},
		{"a/", "a"},
		{"a/b", "a b"},
		{"a:b*c?d\"e<f>g|h", "a b c d e f g h"},
		{"", ""},
	} {
		if got := SanitizeFolder(tc.raw); got != tc.want {
			t.Errorf("SanitizeFolder(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFolderForNeverLeavesTheCategoryTree(t *testing.T) {
	list := Defaults()

	if got := FolderFor(list, "Music"); got != "Music" {
		t.Errorf("Music folder = %q", got)
	}
	// A record written before a category was renamed still has to open
	// somewhere sensible rather than refuse to be shown.
	if got := FolderFor(list, "SomethingRemoved"); got != OtherName {
		t.Errorf("unknown folder = %q, want %q", got, OtherName)
	}
	// And an empty folder name falls back to the catch-all rather than
	// handing back a path that would be the download directory itself.
	if got := FolderFor([]Category{{Name: "X", Folder: ""}}, "X"); got != OtherName {
		t.Errorf("empty folder = %q, want %q", got, OtherName)
	}
}

func TestKnownMatchesByNameOnly(t *testing.T) {
	list := Defaults()

	if !Known(list, "music") {
		t.Error("Known should ignore case")
	}
	if !Known(list, " Music ") {
		t.Error("Known should ignore surrounding space")
	}
	if Known(list, "") {
		t.Error("the empty name is never a category")
	}
	if Known(list, "Musician") {
		t.Error("a name that merely starts with a category is not that category")
	}
}

func TestSaveRoundTripsThroughLoad(t *testing.T) {
	dir := t.TempDir()
	list := Defaults()
	if err := Save(dir, list); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := Load(dir)
	if len(got) != len(list) {
		t.Fatalf("round trip gave %d categories, want %d", len(got), len(list))
	}
	for i := range list {
		if got[i].Name != list[i].Name || got[i].Folder != list[i].Folder {
			t.Errorf("category %d = %+v, want %+v", i, got[i], list[i])
		}
	}
}
