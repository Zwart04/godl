//go:build windows

package tray

import (
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type Action int

const (
	Open Action = iota + 1
	Exit
)

const (
	className       = "GodlTrayWindow"
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	wmContextMenu   = 0x007B
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmCommand       = 0x0111
	wmApp           = 0x8000
	wmTrayCallback  = wmApp + 1
	wmTrayExit      = wmApp + 2
	wmTrayOpen      = wmApp + 3
	wmMenuOpen      = 1
	wmMenuExit      = 2

	nimAdd     = 0
	nimDelete  = 2
	nifMessage = 1
	nifIcon    = 2
	nifTip     = 4

	idiApplication = 32512
	pmRightButton  = 2
	tpmReturnCmd   = 0x0100
	mfString       = 0
)

type wndClass struct {
	Style     uint32
	WndProc   uintptr
	ClsExtra  int32
	WndExtra  int32
	Instance  windows.Handle
	Icon      windows.Handle
	Cursor    windows.Handle
	Brush     windows.Handle
	MenuName  *uint16
	ClassName *uint16
}

type notifyIconData struct {
	CbSize          uint32
	HWnd            windows.Handle
	UID             uint32
	UFlags          uint32
	CallbackMessage uint32
	HIcon           windows.Handle
	SzTip           [128]uint16
	DwState         uint32
	DwStateMask     uint32
	SzInfo          [256]uint16
	SzInfoTitle     [64]uint16
	DwInfoFlags     uint32
	GUIDItem        [16]byte
	BalloonIcon     windows.Handle
}

type Manager struct {
	actions   chan Action
	ready     chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	hwnd      windows.Handle
	closed    bool
	iconAdded bool
}

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procCreate        = user32.NewProc("CreateWindowExW")
	procDestroy       = user32.NewProc("DestroyWindow")
	procDef           = user32.NewProc("DefWindowProcW")
	procRegister      = user32.NewProc("RegisterClassW")
	procGetModule     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procGetMessage    = user32.NewProc("GetMessageW")
	procTranslate     = user32.NewProc("TranslateMessage")
	procDispatch      = user32.NewProc("DispatchMessageW")
	procPost          = user32.NewProc("PostMessageW")
	procNotify        = shell32.NewProc("Shell_NotifyIconW")
	procLoadIcon      = user32.NewProc("LoadIconW")
	procExtractIcon   = shell32.NewProc("ExtractIconW")
	procDestroyIcon   = user32.NewProc("DestroyIcon")
	procCreateMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenu    = user32.NewProc("AppendMenuW")
	procTrackMenu     = user32.NewProc("TrackPopupMenu")
	procSetForeground = user32.NewProc("SetForegroundWindow")
	procGetCursorPos  = user32.NewProc("GetCursorPos")
	procPostQuit      = user32.NewProc("PostQuitMessage")
	windowProc        = windows.NewCallback(wndProc)
	classOnce         sync.Once
	classAtom         uintptr
	current           *Manager // accessed only by the locked tray OS thread
)

type point struct{ X, Y int32 }

type message struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

func New() *Manager {
	m := &Manager{actions: make(chan Action, 8), ready: make(chan struct{}), done: make(chan struct{})}
	go m.run()
	<-m.ready
	return m
}

func (m *Manager) Events() <-chan Action { return m.actions }

func (m *Manager) WindowHandle() uintptr {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return uintptr(m.hwnd)
}

func (m *Manager) RequestExit() {
	if m == nil {
		return
	}
	m.mu.Lock()
	h := m.hwnd
	m.mu.Unlock()
	if h != 0 {
		procPost.Call(uintptr(h), wmTrayExit, 0, 0)
	}
}

func (m *Manager) RequestOpen() {
	if m == nil {
		return
	}
	m.mu.Lock()
	h := m.hwnd
	m.mu.Unlock()
	if h != 0 {
		procPost.Call(uintptr(h), wmTrayOpen, 0, 0)
	}
}

func (m *Manager) IconAdded() bool {
	if m == nil {
		return false
	}
	select {
	case <-m.ready:
	default:
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iconAdded
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	h := m.hwnd
	m.mu.Unlock()
	if h != 0 {
		procPost.Call(uintptr(h), wmClose, 0, 0)
	}
	<-m.done
}

func (m *Manager) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current = m
	defer func() { current = nil; close(m.actions); close(m.done) }()

	classOnce.Do(registerWindowClass)
	name, _ := windows.UTF16PtrFromString(className)
	inst, _, _ := procGetModule.Call(0)
	// A hidden top-level host window keeps Shell_NotifyIcon callbacks routable
	// and lets Windows enumerate the owner for shutdown diagnostics.
	h, _, _ := procCreate.Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0,
		0, 0, inst, 0)
	if h == 0 {
		close(m.ready)
		return
	}
	m.mu.Lock()
	m.hwnd = windows.Handle(h)
	m.mu.Unlock()
	icon, ownedIcon := applicationIcon()
	var data notifyIconData
	data.CbSize = uint32(unsafe.Sizeof(data))
	data.HWnd = windows.Handle(h)
	data.UID = 1
	data.UFlags = nifMessage | nifIcon | nifTip
	data.CallbackMessage = wmTrayCallback
	data.HIcon = icon
	if tip, err := windows.UTF16FromString("godl download manager"); err == nil {
		copy(data.SzTip[:], tip)
	}
	added, _, _ := procNotify.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	if added == 0 {
		if ownedIcon && icon != 0 {
			procDestroyIcon.Call(uintptr(icon))
		}
		close(m.ready)
		procDestroy.Call(h)
		return
	}
	m.mu.Lock()
	m.iconAdded = true
	m.mu.Unlock()
	close(m.ready)

	for {
		var msg message
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslate.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatch.Call(uintptr(unsafe.Pointer(&msg)))
	}
	_, _, _ = procNotify.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
	m.mu.Lock()
	m.iconAdded = false
	m.mu.Unlock()
	if ownedIcon && icon != 0 {
		procDestroyIcon.Call(uintptr(icon))
	}
}

func registerWindowClass() {
	name, _ := windows.UTF16PtrFromString(className)
	inst, _, _ := procGetModule.Call(0)
	wc := wndClass{WndProc: windowProc, Instance: windows.Handle(inst), ClassName: name}
	classAtom, _, _ = procRegister.Call(uintptr(unsafe.Pointer(&wc)))
}

func applicationIcon() (windows.Handle, bool) {
	// ExtractIconW accepts the running executable path and returns its first icon.
	buf := make([]uint16, 32768)
	n, err := windows.GetModuleFileName(windows.Handle(instHandle()), &buf[0], uint32(len(buf)))
	if err == nil && n > 0 {
		icon, _, _ := procExtractIcon.Call(0, uintptr(unsafe.Pointer(&buf[0])), 0)
		if icon != 0 {
			return windows.Handle(icon), true
		}
	}
	icon, _, _ := procLoadIcon.Call(0, idiApplication)
	return windows.Handle(icon), false
}

func instHandle() uintptr {
	h, _, _ := procGetModule.Call(0)
	return h
}

func wndProc(hwnd windows.Handle, msg uint32, wp, lp uintptr) uintptr {
	switch msg {
	case wmTrayCallback:
		switch uint32(lp) {
		case wmLButtonDblClk:
			postAction(Open)
		case wmRButtonUp, wmContextMenu:
			showMenu(hwnd)
		}
		return 0
	case wmTrayExit:
		postAction(Exit)
		return 0
	case wmTrayOpen:
		postAction(Open)
		return 0
	case wmCommand:
		switch wp & 0xffff {
		case wmMenuOpen:
			postAction(Open)
		case wmMenuExit:
			postAction(Exit)
		}
		return 0
	case wmClose:
		procDestroy.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		if current != nil {
			current.mu.Lock()
			current.hwnd = 0
			current.mu.Unlock()
		}
		procPostQuit.Call(0)
		return 0
	}
	r, _, _ := procDef.Call(uintptr(hwnd), uintptr(msg), wp, lp)
	return r
}

func postAction(a Action) {
	if current == nil {
		return
	}
	select {
	case current.actions <- a:
	default:
	}
}

func showMenu(hwnd windows.Handle) {
	menu, _, _ := procCreateMenu.Call()
	if menu == 0 {
		return
	}
	defer user32.NewProc("DestroyMenu").Call(menu)
	open, _ := windows.UTF16PtrFromString("Open godl")
	exit, _ := windows.UTF16PtrFromString("Exit")
	procAppendMenu.Call(menu, mfString, wmMenuOpen, uintptr(unsafe.Pointer(open)))
	procAppendMenu.Call(menu, mfString, wmMenuExit, uintptr(unsafe.Pointer(exit)))
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	procSetForeground.Call(uintptr(hwnd))
	selected, _, _ := procTrackMenu.Call(menu, tpmReturnCmd|pmRightButton, uintptr(int32(p.X)), uintptr(int32(p.Y)), 0, uintptr(hwnd), 0)
	if selected != 0 {
		wndProc(hwnd, wmCommand, selected, 0)
	}
}
