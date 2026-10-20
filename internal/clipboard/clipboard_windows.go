//go:build windows

package clipboard

import (
	"errors"
	"syscall"
	"unsafe"
)

// cfUnicodeText is the clipboard format every application on Windows writes
// text in, so it is the one worth asking for.
const cfUnicodeText = 13

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
)

// ErrHeld reports that another application had the clipboard open at the
// instant it was asked for. That is ordinary, it passes in a moment, and the
// next tick reads it - so it is a reason to skip, not a fault to report.
var ErrHeld = errors.New("the clipboard was held by another application")

// pointerFrom reinterprets an address a syscall handed back as a pointer.
//
// syscall reports results as uintptr, which is an integer and not a
// reference. Converting one straight to unsafe.Pointer is exactly the pattern
// the unsafeptr check exists to flag, and rightly so in general: nothing about
// the number proves it still addresses memory that is alive.
//
// Here it does, and the reason is worth stating rather than working around.
// The address comes from GlobalLock, which pins a clipboard block owned by the
// operating system. It is not Go memory, so the collector neither moves it nor
// reclaims it, and it stays pinned until GlobalUnlock runs at the end of this
// same call. The reinterpretation below is the same reinterpretation a direct
// conversion would perform; it is written as a load from a local so that what
// is being claimed sits next to the reason it holds.
//
//go:nocheckptr
func pointerFrom(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// Windows reads the real clipboard.
type Windows struct{}

// Text reads the clipboard's Unicode text.
//
// It is opened, read and closed within this call and nothing else. Holding it
// any longer would block every other application trying to use the clipboard,
// which is the one way a monitoring process becomes a nuisance to the rest of
// the machine.
func (Windows) Text() (string, error) {
	// A null owner means "whoever gets there first", which is the only thing a
	// process with no window of its own can say. It can lose that race, and
	// losing is reported rather than retried here - there is a next tick.
	if ok, _, _ := procOpenClipboard.Call(0); ok == 0 {
		return "", ErrHeld
	}
	defer procCloseClipboard.Call()

	handle, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if handle == 0 {
		// Nothing on the clipboard, or what is there is not text. Both are the
		// ordinary case and neither is worth mentioning.
		return "", nil
	}

	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return "", errors.New("could not lock the clipboard block")
	}
	defer procGlobalUnlock.Call(handle)

	base := pointerFrom(ptr)

	// The block is NUL-terminated and its length is not handed back, so it is
	// walked rather than trusted. The bound is not a limit on what may be
	// copied - it is what stops a malformed block from walking off the end of
	// memory while looking for the terminator.
	const bound = 1 << 24
	var n int
	for n < bound {
		if *(*uint16)(unsafe.Add(base, uintptr(n)*2)) == 0 {
			break
		}
		n++
	}

	return syscall.UTF16ToString(unsafe.Slice((*uint16)(base), n)), nil
}
