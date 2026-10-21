//go:build windows

// Package foreground brings godl's own window to the front of the desktop.
//
// A link handed over from a browser is a request to act now. The browser is
// still the foreground window at that moment, so a manager that only makes its
// window visible hands the person a window they have to find and click - the
// one thing a download manager exists to spare them. Windows also refuses
// SetForegroundWindow from a process the person is not typing into, so the
// call is made with the calling thread attached to the foreground thread, and
// the window is raised with a topmost/not-topmost pair as a fallback for when
// activation is still refused. The pair leaves nothing permanently on top; it
// only moves the window to the head of the ordinary z-order.
package foreground

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// The tray host is a window of this process too. It has no caption, but
	// naming it here keeps a future tray window that does have one from
	// being brought forward instead of the manager.
	trayClass = "GodlTrayWindow"

	// GWL_EXSTYLE is -20. Spelled as its unsigned bit pattern because a
	// negative constant cannot be converted to uintptr at compile time.
	gwlExStyle  = ^uintptr(19)
	wsExToolWin = 0x00000080

	swShow    = 5
	swRestore = 9

	// HWND_TOPMOST and HWND_NOTOPMOST are -1 and -2.
	hwndTopmost    = ^uintptr(0)
	hwndNotTopmost = ^uintptr(0) - 1

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowLongW           = user32.NewProc("GetWindowLongW")
	procIsIconic                 = user32.NewProc("IsIconic")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procSwitchToThisWindow       = user32.NewProc("SwitchToThisWindow")
	procGetCurrentThreadID       = kernel32.NewProc("GetCurrentThreadId")
)

// scan is the state EnumWindows reads. The enumeration runs on the calling
// thread, so the mutex only keeps a second Show from replacing the target
// halfway through.
type scanState struct {
	sync.Mutex
	pid   uint32
	title string
	found uintptr
}

var scan scanState

var enumProc = windows.NewCallback(func(hwnd, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != scan.pid {
		return 1
	}

	var class [128]uint16
	if n, _, _ := procGetClassNameW.Call(hwnd,
		uintptr(unsafe.Pointer(&class[0])), uintptr(len(class))); n > 0 &&
		windows.UTF16ToString(class[:n]) == trayClass {
		return 1
	}

	// Tool windows are the hidden helpers Wails and WebView2 leave behind;
	// none of them can be the manager's face.
	ex, _, _ := procGetWindowLongW.Call(hwnd, uintptr(gwlExStyle))
	if ex&wsExToolWin != 0 {
		return 1
	}

	var caption [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd,
		uintptr(unsafe.Pointer(&caption[0])), uintptr(len(caption)))
	if n == 0 || windows.UTF16ToString(caption[:n]) != scan.title {
		return 1
	}

	scan.found = hwnd
	return 0 // stop enumerating: the caption is the manager's own
})

// Show makes the window captioned title visible and puts it in front.
//
// It finds the window by caption rather than by handle because the window is
// created after this package would have a chance to be handed one, and it must
// be findable while it is hidden in the tray. A title that matches nothing is
// not an error: this is called from a request handler, and a missing window
// means the window is already gone or was never open.
func Show(title string) {
	hwnd := find(title)
	if hwnd == 0 {
		return
	}

	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	} else {
		procShowWindow.Call(hwnd, swShow)
	}
	raise(hwnd)
}

func find(title string) uintptr {
	scan.Lock()
	defer scan.Unlock()

	scan.pid = uint32(os.Getpid())
	scan.title = title
	scan.found = 0
	procEnumWindows.Call(enumProc, 0)
	return scan.found
}

func raise(hwnd uintptr) {
	flags := uintptr(swpNoSize | swpNoMove | swpShowWindow)
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, flags)
	procSetWindowPos.Call(hwnd, hwndNotTopmost, 0, 0, 0, 0, flags)

	fg, _, _ := procGetForegroundWindow.Call()
	cur, _, _ := procGetCurrentThreadID.Call()

	var fgPid uint32
	var fgThread uintptr
	if fg != 0 {
		fgThread, _, _ = procGetWindowThreadProcessID.Call(fg, uintptr(unsafe.Pointer(&fgPid)))
	}

	attached := false
	if fgThread != 0 && fgThread != cur {
		if ok, _, _ := procAttachThreadInput.Call(fgThread, cur, 1); ok != 0 {
			attached = true
		}
	}

	procBringWindowToTop.Call(hwnd)
	if ok, _, _ := procSetForegroundWindow.Call(hwnd); ok == 0 {
		// The undocumented but still present switch Windows itself uses
		// when a program asks to be activated from the background.
		procSwitchToThisWindow.Call(hwnd, 1)
	}

	if attached {
		procAttachThreadInput.Call(fgThread, cur, 0)
	}
}
