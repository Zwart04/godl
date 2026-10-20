//go:build windows

package notify

import (
	"testing"
	"unicode/utf16"
)

// The shell reads these arrays by fixed size, so a filename that runs past
// the end is not a formatting problem, it is somebody else's memory.
func TestUTF16zNeverExceedsItsBuffer(t *testing.T) {
	long := make([]uint16, 300)
	for i := range long {
		long[i] = 'x'
	}
	s := string(utf16.Decode(long))

	got := utf16z(s, 16)
	if len(got) != 16 {
		t.Fatalf("len = %d, want exactly the 16 the buffer holds", len(got))
	}
	if got[15] != 0 {
		t.Fatal("the last slot is not NUL, so the shell reads past the name")
	}
	for i := 0; i < 15; i++ {
		if got[i] != 'x' {
			t.Fatalf("slot %d = %d, want the truncated name", i, got[i])
		}
	}
}

func TestUTF16zTerminatesWhatItFits(t *testing.T) {
	got := utf16z("debian.iso", 16)
	want := append([]uint16{'d', 'e', 'b', 'i', 'a', 'n', '.', 'i', 's', 'o'}, 0)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (name plus one NUL)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slot %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestUTF16zSaysNothingWhenThereIsNothing(t *testing.T) {
	if got := utf16z("   ", 16); got != nil {
		t.Fatalf("whitespace produced %v, want nil so no empty balloon appears", got)
	}
}
