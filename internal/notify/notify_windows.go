package notify

import (
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A balloon from the shell arrives inside a NOTIFYICONDATAW. Its layout is
// fixed by the Windows ABI rather than by Go, so the struct below is written
// field for field in the order the C header declares them and is checked
// against the size the shell expects before anything is handed over: getting
// this wrong does not fail loudly, it writes a message box over memory that
// is not one.
type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GUIDItem         [16]byte
	HBalloonIcon     windows.Handle
}

// The size of NOTIFYICONDATAW on x64. Anything else means this struct and the
// shell disagree about where the fields are, and a balloon is not worth a
// corrupted write.
const notifyIconDataSize = 976

const (
	nifIcon        = 0x00000002
	nifInfo        = 0x00000010
	nimAdd         = 0x00000000
	nimDelete      = 0x00000002
	niifInfo       = 0x00000001
	idiApplication = 32512
	pmRemove       = 0x00000001

	// Long enough to read the filename and the size, short enough that a
	// burst of small downloads does not leave a queue of balloons behind.
	balloonHold = 6 * time.Second
)

var (
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	user32              = windows.NewLazySystemDLL("user32.dll")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procLoadIcon        = user32.NewProc("LoadIconW")
	procPeekMessage     = user32.NewProc("PeekMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMsg     = user32.NewProc("DispatchMessageW")
)

// HWND_MESSAGE, spelled as the C header spells it: a parent that puts the
// window in the message-only namespace, where nothing but the shell can see
// it. There is no window on screen, only somewhere for the balloon to be
// addressed to.
const hwndMessage = ^uintptr(2)

// winMsg is MSG, laid out the way the shell writes it.
type winMsg struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

func balloon(title, message string) {
	// The window has to be created, pumped and destroyed by one OS thread -
	// Windows ties a window to the thread that made it - and a goroutine is
	// free to move between them at any moment.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if unsafe.Sizeof(notifyIconData{}) != notifyIconDataSize {
		return
	}
	hwnd := newMessageWindow()
	if hwnd == 0 {
		return
	}
	defer destroyWindow(hwnd)

	icon, _, _ := procLoadIcon.Call(0, idiApplication)

	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = nifIcon | nifInfo
	nid.HIcon = windows.Handle(icon)
	nid.DwInfoFlags = niifInfo
	copy(nid.SzInfoTitle[:], utf16z(title, 63))
	copy(nid.SzInfo[:], utf16z(message, 255))

	if shown, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); shown == 0 {
		return
	}
	defer procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

	// The shell delivers the balloon through this window's queue, so the
	// queue has to be drained for it to arrive and then be taken away. The
	// loop holds nothing but this goroutine for a few seconds.
	deadline := time.Now().Add(balloonHold)
	for time.Now().Before(deadline) {
		var msg winMsg
		for got, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&msg)), uintptr(hwnd), 0, 0, pmRemove); got != 0; {
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
			got, _, _ = procPeekMessage.Call(uintptr(unsafe.Pointer(&msg)), uintptr(hwnd), 0, 0, pmRemove)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newMessageWindow() windows.Handle {
	// "STATIC" is a class every Windows process already has registered, so no
	// window procedure of our own is needed for a window nothing will draw.
	name, _ := windows.UTF16PtrFromString("STATIC")
	h, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		0,
		0,
		0, 0, 0, 0,
		hwndMessage,
		0, 0, 0,
	)
	return windows.Handle(h)
}

func destroyWindow(hwnd windows.Handle) {
	procDestroyWindow.Call(uintptr(hwnd))
}

// utf16z converts to a NUL-terminated UTF-16 slice no longer than max,
// truncating on a character boundary rather than letting the shell read past
// the end of a filename that happened to be long.
func utf16z(s string, max int) []uint16 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out, err := windows.UTF16FromString(s)
	if err != nil || len(out) == 0 {
		return nil
	}
	if len(out) > max {
		out = out[:max]
		out[max-1] = 0
	}
	return out
}
