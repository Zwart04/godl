//go:build !windows

package clipboard

// Windows is the reader this package was written for. Anywhere else it reads
// nothing, so the monitor simply never fires.
//
// That is the right answer rather than a gap: the rest of godl is a Windows
// application, and keeping this file means the polling loop - which is the
// part with any logic in it - still compiles and can still be tested on
// whatever machine a developer happens to be on.
type Windows struct{}

// Text always reports an empty clipboard.
func (Windows) Text() (string, error) { return "", nil }
