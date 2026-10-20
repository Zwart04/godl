//go:build windows

package clipboard

import "testing"

// The clipboard belongs to the whole desktop, so nothing is written to it and
// nothing about its contents is asserted. What is asserted is that asking for
// it does not crash, does not hang, and does not report a fault when there is
// nothing there to read - which is the case that would otherwise go unnoticed
// until someone copied a link and nothing happened.
func TestTheRealReaderAnswers(t *testing.T) {
	text, err := (Windows{}).Text()
	if err != nil && err != ErrHeld {
		t.Fatalf("Text: %v", err)
	}
	if IsLink(text) && len(text) > MaxLink {
		t.Fatalf("IsLink accepted %d bytes, more than MaxLink", len(text))
	}
}
