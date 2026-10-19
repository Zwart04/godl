//go:build !windows

package main

import (
	"fmt"
	"os"
)

// notify is the non-Windows counterpart of the MessageBox helper, kept so the
// package still builds when the window code is compiled for another platform.
func notify(caption, message string) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", caption, message)
}
