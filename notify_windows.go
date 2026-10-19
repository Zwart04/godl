//go:build windows

package main

import "golang.org/x/sys/windows"

// notify shows a message to the user.
//
// The windowed subsystem means this process has no console: println and
// fmt.Println to stderr go nowhere a person will ever see them, so an error
// path that only prints looks exactly like a program that worked. Every failure
// before or after the window opens has to be shown this way.
func notify(caption, message string) {
	t, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	// MessageBox is blocking on purpose: it runs before any window exists.
	_, _ = windows.MessageBox(0, m, t, windows.MB_ICONINFORMATION)
}
