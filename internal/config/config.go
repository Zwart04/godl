// Package config holds the paths and secrets shared by every godl process.
//
// The CLI, the background server and the browser's native messaging host all
// have to agree on where state lives and how the host authenticates. Keeping
// that in one place is what stops them from drifting apart.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"godl/internal/state"
)

// EnvHome overrides the configuration directory. Tests and portable installs
// use it; leaving it unset puts everything under the user's config dir.
const EnvHome = "GODL_HOME"

// Version is reported by every surface that answers a version question, so the
// CLI, the UI and the browser host cannot disagree about what is installed.
const Version = "0.3.3"

const (
	tokenFile    = "api-token"
	endpointFile = "api-endpoint"
)

// Dir is the directory holding state, the API token and the endpoint file.
func Dir() string {
	if h := strings.TrimSpace(os.Getenv(EnvHome)); h != "" {
		return h
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "godl")
}

// Store is the download record store.
func Store() state.Store { return state.Store{Dir: filepath.Join(Dir(), "downloads")} }

// FilesDir is where downloads land when the caller names no path.
//
// It has to be absolute: the browser's native messaging host and a background
// server both arrive with whatever working directory they were started in, and
// a relative path would scatter files across two of them. It is deliberately
// not the record directory above - state and payload stay separate.
//
// It also has to be somewhere a person can see. The first version put the
// payload inside the application profile, which is hidden well enough that a
// finished 756 MB download looked like nothing had happened at all: the
// browser had already erased its own row, and godl said no more about it. So
// the files now sit under the user's own Downloads folder, with the category
// rules sorting them into folders underneath.
//
// A run redirected by GODL_HOME keeps everything inside itself, so a test or
// a portable copy never drops a file into the real Downloads folder.
func FilesDir() string {
	if h := strings.TrimSpace(os.Getenv(EnvHome)); h != "" {
		return filepath.Join(h, "files")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "Downloads", "godl")
	}
	return filepath.Join(Dir(), "files")
}

// EnsureFilesDir creates the download directory and returns its path.
//
// Downloads can be unwritable - redirected by policy, on a drive that just
// filled up, or simply deleted along with everything else in it. A download
// that lands somewhere less obvious beats a download that cannot start, so the
// profile is tried before giving up.
func EnsureFilesDir() (string, error) {
	dir := FilesDir()
	if err := os.MkdirAll(dir, 0o700); err == nil {
		return dir, nil
	}
	fallback := filepath.Join(Dir(), "files")
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return "", err
	}
	return fallback, nil
}

func createDir() error { return os.MkdirAll(Dir(), 0o700) }

// Token returns the API secret, generating one on first use. It is written
// once per install and read by the native messaging host, which the browser
// launches with no environment of ours.
func Token() (string, error) {
	path := filepath.Join(Dir(), tokenFile)
	if raw, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(raw)); t != "" {
			return t, nil
		}
	}

	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])

	if err := createDir(); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// WriteEndpoint records where the manager is listening so the native messaging
// host can find it without guessing a port.
func WriteEndpoint(url string) error {
	if err := createDir(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(), endpointFile), []byte(url), 0o600)
}

// ReadEndpoint returns the recorded manager URL, or an empty string.
func ReadEndpoint() string {
	raw, err := os.ReadFile(filepath.Join(Dir(), endpointFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// RemoveEndpoint clears the recorded URL, used when a server shuts down.
func RemoveEndpoint() {
	_ = os.Remove(filepath.Join(Dir(), endpointFile))
}

// Save persists a download record into the store.
func Save(d *state.Download) error {
	if d == nil {
		return errors.New("nothing to save")
	}
	if err := createDir(); err != nil {
		return err
	}
	return Store().Save(d)
}

// IDFromFileName picks a safe filename from a URL when the server proposed
// nothing. Query strings and fragments never belong in a filename.
func IDFromFileName(rawURL string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if u == "" {
		return ""
	}
	name := filepath.Base(strings.ReplaceAll(u, "\\", "/"))
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	return name
}

// DescribeTokenError turns a missing directory into an actionable message.
func DescribeTokenError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w (run \"godl serve\" once to create it)", err)
	}
	return err
}
