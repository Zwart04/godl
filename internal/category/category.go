// Package category sorts downloads into folders by what they are.
//
// A category is three things: a name the user recognises, the folder files of
// that kind land in, and the extensions that say a file belongs to it. Keeping
// them together is what lets "this is a program" and "put it in Programs" be
// the same decision, made once, at the moment the link arrives.
//
// The list is built in and can be changed by hand. Nothing here touches the
// filesystem beyond reading that file - the folders themselves are created by
// whoever is about to write into them.
package category

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the per-user override, kept beside the rest of godl's state.
const FileName = "categories.json"

// OtherName is the catch-all. Every list ends with one, because a file whose
// extension says nothing has to have somewhere to go.
const OtherName = "Other"

// Category is one kind of file and the folder it belongs in.
type Category struct {
	Name string `json:"name"`
	// Folder is a single directory name, not a path. It is joined to the
	// download directory by whoever writes into it, so anything that looks
	// like a path is rejected rather than trusted.
	Folder string `json:"folder"`
	// Extensions are the lower-case forms, dot included. Empty means the
	// category is only ever reached by being chosen directly.
	Extensions []string `json:"extensions,omitempty"`
}

// Defaults is the list godl starts with, in the order it is shown.
//
// The grouping is the one a download manager's user already has in their head:
// things to run, things to open later, things to read, and so on. It is a
// starting point, not a rule - the whole list can be replaced or added to.
func Defaults() []Category {
	return []Category{
		{
			Name:   "Programs",
			Folder: "Programs",
			// Installers and disc images. An ISO is here rather than with the
			// archives because it is almost always something to run.
			Extensions: []string{".exe", ".msi", ".msix", ".appx", ".appxbundle", ".iso", ".img", ".apk", ".deb", ".rpm", ".pkg", ".dmg"},
		},
		{
			Name:       "Archives",
			Folder:     "Archives",
			Extensions: []string{".zip", ".7z", ".rar", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".txz", ".zst", ".cab"},
		},
		{
			Name:   "Documents",
			Folder: "Documents",
			Extensions: []string{
				".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".odt", ".ods",
				".txt", ".rtf", ".csv", ".epub", ".mobi", ".md",
			},
		},
		{
			Name:       "Music",
			Folder:     "Music",
			Extensions: []string{".mp3", ".flac", ".wav", ".aac", ".ogg", ".oga", ".m4a", ".wma", ".opus", ".aiff"},
		},
		{
			Name:   "Video",
			Folder: "Video",
			Extensions: []string{
				".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm",
				".m4v", ".mpg", ".mpeg", ".ts", ".vob",
			},
		},
		{
			Name:   "Pictures",
			Folder: "Pictures",
			Extensions: []string{
				".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".webp",
				".heic", ".tif", ".tiff", ".ico", ".raw",
			},
		},
		{Name: OtherName, Folder: OtherName},
	}
}

// Load returns the categories in force.
//
// A categories.json is folded over the built-in list: an entry whose name
// already exists replaces it, and one with a new name is added. That means the
// file only has to hold what a user wants changed - nobody should have to copy
// out the whole default list to add one folder of their own.
//
// A file that cannot be read or understood falls back to the built-ins. A
// settings file must never be able to stop downloads landing somewhere.
func Load(dir string) []Category {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return Defaults()
	}
	var over []Category
	if err := json.Unmarshal(raw, &over); err != nil {
		return Defaults()
	}
	return merge(Defaults(), over)
}

// Save writes a list so it can be edited by hand later.
func Save(dir string, list []Category) error {
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FileName), raw, 0o600)
}

func merge(built, over []Category) []Category {
	// The catch-all is held back so it can always be put last, which is where
	// anything unmatched has to land.
	var fallback Category
	rest := make([]Category, 0, len(built)+len(over))
	for _, c := range built {
		if strings.EqualFold(c.Name, OtherName) {
			fallback = normalize(c)
			continue
		}
		rest = append(rest, normalize(c))
	}
	if fallback.Name == "" {
		fallback = Category{Name: OtherName, Folder: OtherName}
	}

	index := make(map[string]int, len(rest))
	for i, c := range rest {
		index[strings.ToLower(c.Name)] = i
	}

	for _, c := range over {
		c = normalize(c)
		if c.Name == "" {
			continue
		}
		if strings.EqualFold(c.Name, OtherName) {
			// The catch-all keeps its name. Anything unmatched still has to
			// arrive under the name the rest of the app asks for by.
			c.Name = OtherName
			fallback = c
			continue
		}
		if i, ok := index[strings.ToLower(c.Name)]; ok {
			rest[i] = c
			continue
		}
		index[strings.ToLower(c.Name)] = len(rest)
		rest = append(rest, c)
	}

	out := append(rest, fallback)

	// A later claim wins. A category the user added naming .iso means .iso is
	// theirs now, and the built-in that also listed it has to stop - otherwise
	// the order of the list decides what a file is rather than the person who
	// wrote the entry down, and adding a category would silently do nothing.
	claimed := map[string]bool{}
	for i := len(out) - 1; i >= 0; i-- {
		kept := make([]string, 0, len(out[i].Extensions))
		for _, ext := range out[i].Extensions {
			if claimed[ext] {
				continue
			}
			claimed[ext] = true
			kept = append(kept, ext)
		}
		out[i].Extensions = kept
	}
	return out
}

func normalize(c Category) Category {
	c.Name = strings.TrimSpace(c.Name)
	c.Folder = SanitizeFolder(c.Folder)
	if c.Folder == "" {
		c.Folder = SanitizeFolder(c.Name)
	}
	out := make([]string, 0, len(c.Extensions))
	for _, ext := range c.Extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		out = append(out, ext)
	}
	c.Extensions = out
	return c
}

// Assign picks the category a filename belongs to, by its extension alone.
//
// It answers with a name rather than a folder so the result can be stored on
// the record and shown to the user; the folder is looked up when it is needed.
func Assign(list []Category, filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return OtherName
	}
	for _, c := range list {
		if strings.EqualFold(c.Name, OtherName) {
			continue
		}
		for _, have := range c.Extensions {
			if have == ext {
				return c.Name
			}
		}
	}
	return OtherName
}

// Known reports whether a list contains this category, matched by name.
func Known(list []Category, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, c := range list {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// FolderFor returns the directory a category's files land in.
//
// An unknown name falls through to the catch-all rather than erroring: a
// record written before a category was renamed should still open somewhere
// sensible instead of refusing to be shown.
func FolderFor(list []Category, name string) string {
	name = strings.TrimSpace(name)
	for _, c := range list {
		if strings.EqualFold(c.Name, name) && c.Folder != "" {
			return c.Folder
		}
	}
	return OtherName
}

// SanitizeFolder keeps a folder name inside the directory it will be joined to.
//
// The name comes from a file the user may have written, and it is joined to a
// path they chose - so a separator or a traversal in it could put a download
// somewhere else on the machine entirely.
//
// Traversal is dropped rather than disguised. Replacing the separators with
// spaces would leave ".." sitting in the middle of a name that merely looks
// harmless, and Windows trims trailing dots and spaces from a path component
// before opening it - so ".. " would be written here as a folder name and read
// back by the filesystem as the parent directory.
func SanitizeFolder(raw string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(raw), func(r rune) bool {
		return r == '/' || r == '\\' || r == ':' || r == 0
	})

	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		// Windows trims these before opening a component, so a name ending in
		// them would not survive the round trip unchanged.
		part = strings.TrimRight(part, " .")
		part = strings.Map(func(r rune) rune {
			switch r {
			case '*', '?', '"', '<', '>', '|':
				return ' '
			}
			return r
		}, part)
		part = strings.Join(strings.Fields(part), " ")
		if part == "" || part == "." || part == ".." {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, " ")
}
