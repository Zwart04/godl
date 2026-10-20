//go:build !windows

package notify

// balloon has no counterpart outside Windows: the shell notification used
// here is one, and pretending to show one would be worse than saying nothing.
func balloon(title, message string) {}
