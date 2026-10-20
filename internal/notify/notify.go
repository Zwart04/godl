// Package notify tells the person that a download finished.
//
// A finished download is invisible work unless something says so. The window
// may be behind four others, and by the time the last byte lands the browser
// has already erased its own row - so a 756 MB transfer that took thirty
// seconds and eight connections looks, from the chair in front of it, exactly
// like nothing happened at all. That is not a cosmetic complaint; it is the
// difference between a download manager working and appearing not to.
//
// A balloon from the shell is used rather than a dialog. It arrives without
// stealing focus, needs no click, and disappears on its own - which matters
// because a manager that interrupts you for every small file is one people
// learn to close.
package notify

// Completed shows one balloon for a finished download.
//
// It returns immediately. The balloon lives on its own goroutine with its own
// short-lived message window, so nothing about a transfer is ever waiting on
// the shell to answer.
func Completed(title, message string) {
	if title == "" && message == "" {
		return
	}
	go balloon(title, message)
}
