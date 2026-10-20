//go:build windows

package status

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"godl/internal/engine"
	"godl/internal/queue"
	"godl/internal/state"
)

// debugf appends one line to whatever file GODL_STATUS_LOG names, if any. The
// window thread has no console to complain on: when a dialog fails to appear,
// this is the only account of why. It is silent unless asked for, and costs a
// getenv on the paths that matter.
func debugf(format string, args ...any) {
	path := os.Getenv("GODL_STATUS_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// The transfer dialog. Everything about a window is decided by the thread
// that made it, so one goroutine owns every dialog in this package, pumps
// their messages, and is the only thing that ever touches them - callers ask
// it to show something, and that is the whole of their involvement.

const (
	// The class name is deliberately not "godl": the smoke suite finds a
	// dialog by class, and a class it shares with the manager's own window
	// would be a guess rather than an identification.
	windowClass = "GodlStatusWindow"

	// Posted to the window thread to say there is work waiting. It has to be
	// posted, because the caller may be any goroutine in the process and
	// Windows ties a window to the thread that created it.
	wmWake = 0x0400 + 42 // WM_USER + 42, a range this class has to itself
	timer  = 1

	// How often the dialog re-reads the queue. A fifth of a second is fast
	// enough that a rate does not look steppy and slow enough that eight
	// open dialogs do not spend their time in the queue's lock.
	refreshEvery = 150
)

// Window styles and messages, spelled the way the headers spell them.
const (
	wsChild      = 0x40000000
	wsVisible    = 0x10000000
	wsTabStop    = 0x00010000
	wsGroup      = 0x00020000
	wsVScroll    = 0x00200000
	wsOverlapped = 0x00cf0000 // WS_OVERLAPPED|CAPTION|SYSMENU|THICKFRAME|MINIMIZEBOX|MAXIMIZEBOX

	ssEndEllipsis   = 0x00004000
	bsAutoRadio     = 0x00000009
	cbsDropDownList = 0x00000003
	bmSetCheck      = 0x00f1
	bstChecked      = 1

	wmNcCreate       = 0x0081
	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmPaint          = 0x000f
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmSetFont        = 0x0030
	wmCommand        = 0x0111
	wmCtlColorStatic = 0x0138
	wmTimer          = 0x0113
	wmLButtonDown    = 0x0201

	cbAddString = 0x0143
	cbSetCurSel = 0x014e
	cbGetCurSel = 0x0147
	bnClicked   = 0 // the high word of a control's notification says a click

	swpNoMove   = 0x0002
	swpNoZOrder = 0x0004
	swShowNA    = 8
	swHide      = 0

	// Text drawing.
	dtSingleline  = 0x00000020
	dtCenter      = 0x00000001
	dtVCenter     = 0x00000004
	dtEndEllipsis = 0x00008000
	dtNoPrefix    = 0x00000800
	dtCalcrect    = 0x00000400
	transparent   = 1

	// Fonts.
	fwNormal         = 400
	defaultCharset   = 1
	cleartypeQuality = 5
	nullStock        = 5
	logPixelsX       = 88
	idcArrow         = 32512
	smCxScreen       = 0
	smCyScreen       = 1
)

// Control identities. They are also the keys of a dialog's last-written text:
// a refresh that changes nothing must not repaint a dozen statics six times a
// second for nothing.
const (
	idAddress = 1001 + iota
	idStatus
	idFileSize
	idDownloaded
	idRate
	idETA
	idResume
)

const (
	idLimitLabel = 1031 + iota
	idLimitCombo
	idLimitCurrent
	idLimitApply
)

const (
	idOptClose = 1041 + iota
	idOptKeep
	idOptFolder
)

const (
	idBtnHide = 1051 + iota
	idBtnPause
	idBtnCancel
)

const (
	idLblStatus = 1101 + iota
	idLblSize
	idLblDown
	idLblRate
	idLblETA
	idLblResume
)

// The two tabs' worth of rows are separate groups so switching tabs is a
// matter of showing one and hiding the other.
const (
	tabStatus = iota
	tabLimiter
	tabCompletion
	tabCount
)

// The layout in logical pixels at 96 DPI. Everything the dialog draws is
// measured from these, then scaled once for the screen it lands on - a second
// guess per control would be a second place for them to disagree.
const (
	marginL  = 12
	tabTop   = 8
	tabH     = 24
	addrY    = 44
	rowY     = 64
	rowH     = 19
	barY     = 188
	barH     = 14
	btnY     = 214
	btnH     = 26
	stripY   = 254
	stripH   = 16
	hdrY     = 282
	hdrH     = 14
	rowsY    = 302
	lineH    = 15
	clientW  = 424
	labelW   = 110
	valueGap = 6
)

// The ceiling menu, built from the same speeds the web table offers so the
// two places cannot teach different numbers for the same button.
var limitPresets = []struct {
	label string
	bps   int64
}{
	{"No limit", 0},
	{"128 KB/s", 128 * 1024},
	{"512 KB/s", 512 * 1024},
	{"1 MB/s", 1024 * 1024},
	{"5 MB/s", 5 * 1024 * 1024},
	{"10 MB/s", 10 * 1024 * 1024},
}

var tabNames = []string{"Download status", "Speed Limiter", "Options on completion"}

// Labels down the left of the status page, in the order they are drawn. They
// are the questions a person asks about a transfer; the answers beside them
// change and these do not.
var rowLabels = []struct {
	id   int
	text string
}{
	{idLblStatus, "Status"},
	{idLblSize, "File size"},
	{idLblDown, "Downloaded"},
	{idLblRate, "Transfer rate"},
	{idLblETA, "Time left"},
	{idLblResume, "Resume capability"},
}

// The dialog's palette, as COLORREF: 0x00BBGGRR.
func colorRef(r, g, b byte) uint32 {
	return uint32(r) | uint32(g)<<8 | uint32(b)<<16
}

var (
	colorWhite  = colorRef(0xFF, 0xFF, 0xFF)
	colorLabel  = colorRef(0x86, 0x86, 0x8B) // #86868B, the quiet half
	colorValue  = colorRef(0x1D, 0x1D, 0x1F) // #1D1D1F
	colorAccent = colorRef(0x0A, 0x84, 0xFF) // #0A84FF, what is happening now
	colorError  = colorRef(0xD7, 0x00, 0x15) // #D70015
	colorDone   = colorRef(0x24, 0x8A, 0x3D) // #248A3D, for the last word
	colorTrack  = colorRef(0xE5, 0xE5, 0xEA) // #E5E5EA, what has not arrived
	colorGreen  = colorRef(0x34, 0xC7, 0x59) // #34C759, what has
	colorHair   = colorRef(0xF0, 0xF0, 0xF0) // #F0F0F0, row separators
)

// rect is RECT, laid out the way the header declares it.
type rect struct {
	L, T, R, B int32
}

// winMsg is MSG on x64: handle, message, two pointers, time and point.
type winMsg struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// paintStruct is PAINTSTRUCT. Only Hdc is read and only the size matters to
// BeginPaint, so the size is asserted before the first dialog is made: an
// arrangement that disagrees with the header would not fail loudly, it would
// write the first dialog's paint over memory that is not a paint.
type paintStruct struct {
	Hdc         windows.Handle
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

const paintStructSize = 72

// wndClass is WNDCLASSW.
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

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClass     = user32.NewProc("RegisterClassW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procShowWindow        = user32.NewProc("ShowWindow")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procMoveWindow        = user32.NewProc("MoveWindow")
	procGetClientRect     = user32.NewProc("GetClientRect")
	procInvalidateRect    = user32.NewProc("InvalidateRect")
	procAdjustWindowRect  = user32.NewProc("AdjustWindowRectEx")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procSetWindowText     = user32.NewProc("SetWindowTextW")
	procGetWindowText     = user32.NewProc("GetWindowTextW")
	procSendMessage       = user32.NewProc("SendMessageW")
	procGetDlgCtrlID      = user32.NewProc("GetDlgCtrlID")
	procSetForeground     = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop  = user32.NewProc("BringWindowToTop")
	procSetTimer          = user32.NewProc("SetTimer")
	procKillTimer         = user32.NewProc("KillTimer")
	procBeginPaint        = user32.NewProc("BeginPaint")
	procEndPaint          = user32.NewProc("EndPaint")
	procFillRect          = user32.NewProc("FillRect")
	procDrawText          = user32.NewProc("DrawTextW")
	procLoadCursor        = user32.NewProc("LoadCursorW")
	procGetDC             = user32.NewProc("GetDC")
	procReleaseDC         = user32.NewProc("ReleaseDC")
	procGetDpiForSystem   = user32.NewProc("GetDpiForSystem")
	procGetMessage        = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessage   = user32.NewProc("DispatchMessageW")
	procPostThreadMessage = user32.NewProc("PostThreadMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")

	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")

	procCreateFont    = gdi32.NewProc("CreateFontW")
	procDeleteObject  = gdi32.NewProc("DeleteObject")
	procSelectObject  = gdi32.NewProc("SelectObject")
	procSetTextColor  = gdi32.NewProc("SetTextColor")
	procSetBkMode     = gdi32.NewProc("SetBkMode")
	procGetStock      = gdi32.NewProc("GetStockObject")
	procCreateBrush   = gdi32.NewProc("CreateSolidBrush")
	procGetDeviceCaps = gdi32.NewProc("GetDeviceCaps")
)

// theme is the set of fonts and brushes every dialog on this thread draws
// with. They belong to the thread the same windows do, and are made once when
// it starts and dropped once when it stops - one per dialog would be a small
// leak per download.
type theme struct {
	ui, small windows.Handle
	brushBody windows.Handle
	brushWalk windows.Handle // the progress track
	brushBar  windows.Handle // the progress fill
	brushHair windows.Handle
	brushWin  windows.Handle // what statics are told to paint behind them
}

var (
	classOnce sync.Once
	th        theme
	wndProcID = windows.NewCallback(wndProc)

	// dialogs and creating are touched by the window thread only.
	dialogs  = map[windows.Handle]*dlg{}
	creating *dlg
)

// Manager owns the transfer dialogs.
type Manager struct {
	q *queue.Queue

	mu sync.Mutex
	// started says a window thread exists, closed says no more may be asked
	// for, and quitting is what that thread looks for when it is woken - the
	// thread ends its own loop rather than taking a WM_QUIT from wherever the
	// caller happens to be standing.
	started  bool
	closed   bool
	quitting bool
	threadID uint32
	ready    chan struct{}
	pending  []string
	done     chan struct{}
}

// New prepares to show transfer dialogs for one queue. Nothing is started
// until the first one is asked for: a manager that sits idle should own no
// thread, no window class and no fonts.
func New(q *queue.Queue) *Manager {
	return &Manager{q: q, done: make(chan struct{})}
}

// Open puts the dialog for one download on the screen, or brings it forward if
// it is already up.
//
// It is safe from anywhere - the queue's event goroutine, the API, a click -
// and does the least it can where it is called: the window thread does the
// reading, the sizing and the making.
func (m *Manager) Open(id string) {
	if m == nil || m.q == nil || id == "" {
		return
	}
	debugf("Open(%q): pid=%d", id, os.Getpid())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if !m.started {
		m.started = true
		m.ready = make(chan struct{})
		go m.run(m.ready)
	}
	ready := m.ready
	m.pending = append(m.pending, id)
	m.mu.Unlock()

	// The thread cannot be told about work before it exists to be told.
	<-ready

	m.mu.Lock()
	tid := m.threadID
	m.mu.Unlock()
	if tid != 0 {
		procPostThreadMessage.Call(uintptr(tid), uintptr(wmWake), 0, 0)
	}
}

// CloseAll puts every dialog down and waits until the thread that owned them
// has gone.
//
// It is called on the way out, and the wait matters: a process that exits
// while its window thread still runs does not care, but a test or a supervisor
// that starts the next thing immediately would find dialogs with no one to
// answer their buttons still standing on the desktop.
func (m *Manager) CloseAll() {
	if m == nil {
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	started, ready := m.started, m.ready
	m.mu.Unlock()

	if !started {
		return
	}
	<-ready
	// The thread id is read after the thread has said it is ready. Reading it
	// before would race the very line that sets it: a zero posted to nobody
	// leaves this caller waiting for a thread that never heard it.
	m.mu.Lock()
	tid := m.threadID
	m.quitting = true
	m.mu.Unlock()
	debugf("CloseAll: asking thread %d to quit", tid)
	if tid != 0 {
		procPostThreadMessage.Call(uintptr(tid), uintptr(wmWake), 0, 0)
	}
	// Waiting is for tidiness, not for correctness. A shutdown that blocks on
	// a window thread is worse than one that leaves the last frame for the
	// process to take with it, and the caller behind this one - removing the
	// endpoint file, releasing the lock - must not be held up by it.
	select {
	case <-m.done:
		debugf("CloseAll: thread finished")
	case <-time.After(3 * time.Second):
		debugf("CloseAll: thread did not finish in 3s, leaving it to the process")
	}
}

// run is the window thread. It is locked to its OS thread for as long as it
// lives because Windows ties every window it makes to that thread, and a
// goroutine is free to move between them at any moment.
func (m *Manager) run(ready chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if unsafe.Sizeof(paintStruct{}) != paintStructSize {
		// Not a quirk worth reporting to anybody: no dialog is made, rather
		// than one that paints over memory it does not own.
		debugf("run: paintStruct size %d != %d, no dialogs will be made", unsafe.Sizeof(paintStruct{}), paintStructSize)
		close(ready)
		close(m.done)
		return
	}

	registerClass()
	makeTheme(systemDPI())

	m.mu.Lock()
	m.threadID = uint32(windows.GetCurrentThreadId())
	m.mu.Unlock()
	debugf("run: thread %d ready (dpi=%d)", windows.GetCurrentThreadId(), systemDPI())
	close(ready)

	var msg winMsg
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 is WM_QUIT, -1 an error; both end the loop
			break
		}
		if msg.Message == wmWake {
			m.mu.Lock()
			quit := m.quitting
			m.mu.Unlock()
			if quit {
				// Every window comes down before the thread that answers for
				// it does. A window that outlives that thread still draws and
				// still takes clicks, and no message sent to it is ever
				// handled again - a ghost is worse than a closed window.
				for hwnd := range dialogs {
					stale := hwnd
					procDestroyWindow.Call(uintptr(stale))
				}
				// Asked from inside its own thread, which is the only place
				// PostQuitMessage is allowed to mean anything.
				procPostQuitMessage.Call(0)
			} else {
				m.createPending()
			}
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	// Every dialog is on this thread. The thread is going away, so they go
	// with it rather than being left on the desktop with nobody inside.
	for hwnd := range dialogs {
		procDestroyWindow.Call(uintptr(hwnd))
	}
	dropTheme()
	debugf("run: message loop ended, thread going away")
	close(m.done)
}

// createPending makes whatever was asked for since the last wake-up. Duplicates
// collapse here rather than at the caller: two events for one download in the
// same tick should be one window, not two.
func (m *Manager) createPending() {
	m.mu.Lock()
	ids := m.pending
	m.pending = nil
	m.mu.Unlock()

	for _, id := range ids {
		debugf("createPending: %q", id)
		if existing := findByID(id); existing != nil {
			procBringWindowToTop.Call(uintptr(existing.hwnd))
			continue
		}
		m.create(id)
	}
}

// findByID finds a dialog already on screen. The map is this thread's alone.
func findByID(id string) *dlg {
	for _, d := range dialogs {
		if d.id == id {
			return d
		}
	}
	return nil
}

// create reads the transfer once and makes its window.
//
// Reading first means the dialog is never shown empty: a window that appeared
// with no numbers in it would look like a failure for as long as it took the
// first timer to arrive, which is a long time to look broken.
func (m *Manager) create(id string) {
	var seed *queue.Snapshot
	for _, s := range m.q.Snapshot() {
		if s.ID == id {
			snap := s
			seed = &snap
			break
		}
	}
	if seed == nil {
		// Removed between the event and now. There is nothing to show and
		// nobody to ask, so no window is better than one with a stale record.
		debugf("create(%q): no snapshot, no window", id)
		return
	}

	rows := len(seed.Progress.Segments)
	if rows == 0 {
		rows = seed.Connections
	}
	if rows < 1 {
		rows = 1
	}
	if rows > 32 {
		rows = 32 // a plan wider than this has stopped being a picture
	}

	d := &dlg{
		q:      m.q,
		id:     id,
		dpi:    systemDPI(),
		seed:   seed,
		last:   map[int]string{},
		tabBox: make([]rect, tabCount),
	}

	clientW := d.s(clientW)
	clientH := d.s(rowsY + rows*lineH + marginL)
	outW, outH := outerSize(d.dpi, clientW, clientH)

	x := d.s(90) + (len(dialogs)*24)%d.s(260)
	y := d.s(70) + (len(dialogs)*24)%d.s(190)

	creating = d
	defer func() { creating = nil }()

	cls, _ := windows.UTF16PtrFromString(windowClass)
	title, _ := windows.UTF16PtrFromString(TitleBar(*seed))
	hwnd, _, cerr := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(title)),
		wsOverlapped,
		uintptr(int32(x)), uintptr(int32(y)),
		uintptr(int32(outW)), uintptr(int32(outH)),
		0, 0,
		uintptr(windowsInst()),
		0,
	)
	if hwnd == 0 {
		debugf("create(%q): CreateWindowEx failed err=%v (inst=%v size=%dx%d client=%dx%d)", id, cerr, windowsInst(), outW, outH, clientW, clientH)
		return
	}
	debugf("create(%q): hwnd=%v size=%dx%d", id, hwnd, outW, outH)
	// Laid out explicitly rather than trusting WM_SIZE to have arrived: a
	// window that is not yet visible is not guaranteed to have been told its
	// own size, and a first paint with no layout would put everything at 0,0.
	var rc rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	d.layout(int(rc.R), int(rc.B))

	procShowWindow.Call(hwnd, swShowNA)
	procBringWindowToTop.Call(hwnd)
	procSetForeground.Call(hwnd)
}

// outerSize turns a client size into the size of the window that contains it,
// which is what CreateWindowEx is asking for.
func outerSize(dpi, clientW, clientH int) (int, int) {
	var r rect
	r.R, r.B = int32(clientW), int32(clientH)
	procAdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), wsOverlapped, 0, 0)
	return int(r.R - r.L), int(r.B - r.T)
}

// dlg is one dialog: its window, its controls, its choices, and the last thing
// it said to each control. All of it is touched by the window thread alone.
type dlg struct {
	hwnd windows.Handle
	q    *queue.Queue
	id   string

	dpi int

	tab    int
	hidden bool
	fullH  int // client height with the details shown, for un-hiding

	keepOpen   bool
	openFolder bool
	revealed   bool

	status  state.Status
	note    string // an action that did not take, shown until the state moves
	notedAt state.Status

	percent float64
	segs    []engine.SegState
	total   int64

	seed      *queue.Snapshot
	last      map[int]string
	tabBox    []rect
	cancelVis bool
	ticked    bool
	painted   bool

	// What this dialog draws rather than owns, in client coordinates. They
	// are measured once per layout rather than per paint, because the click
	// handler has to know where the tabs are before anything is drawn.
	bar, strip, hdr, rowsRect rect
	statusColor               uint32

	// Controls, kept as handles because the dialog creates them and then only
	// ever talks to them.
	addr, statusH, sizeH, downH, rateH, etaH, resumeH windows.Handle
	combo, current                                    windows.Handle
	btnPause, btnHide, btnCancel                      windows.Handle
	groupA, groupB, groupC                            []windows.Handle
	labels                                            []windows.Handle
}

// s scales a logical pixel to the screen the dialog is on.
func (d *dlg) s(v int) int {
	return int(float64(v)*float64(d.dpi)/96.0 + 0.5)
}

// build makes the controls. It runs during WM_CREATE, while CreateWindowEx is
// still on the stack, so the dialog is fully populated before the window is
// ever shown.
func (d *dlg) build() {
	// The status page: one address across the top, then the questions. The
	// labels wear the small face and the answers the full one, so the eye
	// lands on what changes.
	d.addr = d.add("STATIC", "", ssEndEllipsis, idAddress, th.ui)

	for _, row := range rowLabels {
		d.labels = append(d.labels, d.add("STATIC", row.text, 0, row.id, th.small))
	}
	d.statusH = d.add("STATIC", "", 0, idStatus, th.ui)
	d.sizeH = d.add("STATIC", "", 0, idFileSize, th.ui)
	d.downH = d.add("STATIC", "", 0, idDownloaded, th.ui)
	d.rateH = d.add("STATIC", "", 0, idRate, th.ui)
	d.etaH = d.add("STATIC", "", 0, idETA, th.ui)
	d.resumeH = d.add("STATIC", "", 0, idResume, th.ui)
	d.groupA = append([]windows.Handle{d.addr}, d.labels...)
	d.groupA = append(d.groupA, d.statusH, d.sizeH, d.downH, d.rateH, d.etaH, d.resumeH)

	// Speed Limiter. The list is filled here so the menu is built from the
	// same speeds the web table offers, in the same order.
	limLabel := d.add("STATIC", "Limit this download to", 0, idLimitLabel, th.small)
	d.combo = d.add("COMBOBOX", "", wsVScroll|wsTabStop|cbsDropDownList, idLimitCombo, th.ui)
	d.current = d.add("STATIC", "", 0, idLimitCurrent, th.small)
	limApply := d.add("BUTTON", "Apply", wsTabStop, idLimitApply, th.ui)
	for _, opt := range limitPresets {
		p, _ := windows.UTF16FromString(opt.label)
		procSendMessage.Call(uintptr(d.combo), cbAddString, 0, uintptr(unsafe.Pointer(&p[0])))
	}
	d.groupB = []windows.Handle{limLabel, d.combo, d.current, limApply}

	// Options on completion. Auto-radio, so choosing one takes the other off
	// without this window having to notice; the first is ticked because it is
	// the default and a menu of choices with nothing chosen looks unfinished.
	optClose := d.add("BUTTON", "Close this window when it finishes", wsTabStop|wsGroup|bsAutoRadio, idOptClose, th.ui)
	optKeep := d.add("BUTTON", "Keep it open so the final numbers can be read", wsTabStop|bsAutoRadio, idOptKeep, th.ui)
	optFolder := d.add("BUTTON", "Open the folder when it finishes", wsTabStop|bsAutoRadio, idOptFolder, th.ui)
	procSendMessage.Call(uintptr(optClose), bmSetCheck, bstChecked, 0)
	d.groupC = []windows.Handle{optClose, optKeep, optFolder}

	// The three answers at the bottom, which belong to no tab: whatever else
	// is being looked at, stopping the transfer must stay one button away.
	// Hide also closes the radio group above it, which is why it carries the
	// group flag even though it is not a choice about anything.
	d.btnHide = d.add("BUTTON", "Hide details", wsTabStop|wsGroup, idBtnHide, th.ui)
	d.btnPause = d.add("BUTTON", "Pause", wsTabStop, idBtnPause, th.ui)
	d.btnCancel = d.add("BUTTON", "Cancel", wsTabStop, idBtnCancel, th.ui)

	procSetTimer.Call(uintptr(d.hwnd), timer, refreshEvery, 0)
	d.showTab(d.tab)

	if d.seed != nil {
		d.apply(*d.seed)
		d.seed = nil
	}
}

// add creates one child control. The class decides what it is; everything
// else - where, what it says, and which font wears it - comes from the caller.
func (d *dlg) add(class, text string, style, id int, font windows.Handle) windows.Handle {
	cls, _ := windows.UTF16PtrFromString(class)
	var t *uint16
	if text != "" {
		t, _ = windows.UTF16PtrFromString(text)
	}
	h, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(t)),
		uintptr(style)|wsChild|wsVisible,
		0, 0, 0, 0,
		uintptr(d.hwnd), uintptr(id), 0, 0,
	)
	hwnd := windows.Handle(h)
	if hwnd != 0 && font != 0 {
		procSendMessage.Call(uintptr(hwnd), wmSetFont, uintptr(font), 1)
	}
	return hwnd
}

// showTab puts one page's controls on the screen and takes the others off.
// Hiding rather than covering means the inactive pages cannot be clicked
// through, which is the difference between a tab and a suggestion.
func (d *dlg) showTab(n int) {
	if n < 0 || n >= tabCount {
		return
	}
	d.tab = n
	groups := [][]windows.Handle{d.groupA, d.groupB, d.groupC}
	for i, g := range groups {
		show := uintptr(swShowNA)
		if i != n {
			show = swHide
		}
		for _, h := range g {
			if h != 0 {
				procShowWindow.Call(uintptr(h), show)
			}
		}
	}
	if d.hwnd != 0 {
		procInvalidateRect.Call(uintptr(d.hwnd), 0, 0)
	}
}

// layout puts every control where it belongs for a client area of w by h.
// It runs on creation and on every resize, so the window can be dragged about
// without the pieces staying behind where they were.
func (d *dlg) layout(w, h int) {
	if !d.hidden {
		d.fullH = h
	}

	m := d.s(marginL)
	labelW := d.s(labelW)
	valueX := m + labelW + d.s(valueGap)
	rowTop := d.s(rowY)
	rowStep := d.s(rowH)

	d.move(d.addr, m, d.s(addrY), w-2*m, d.s(16))
	for i, lbl := range d.labels {
		d.move(lbl, m, rowTop+i*rowStep, labelW, d.s(16))
	}
	values := []struct {
		h windows.Handle
	}{
		{d.statusH}, {d.sizeH}, {d.downH}, {d.rateH}, {d.etaH}, {d.resumeH},
	}
	for i, v := range values {
		d.move(v.h, valueX, rowTop+i*rowStep, w-valueX-m, d.s(16))
	}

	// Speed Limiter. The combo's own rectangle reaches well below the line it
	// shows - that is where a drop-down list lives - so everything else on
	// this page sits beside it rather than under it.
	comboW := d.s(190)
	d.move(d.find(idLimitLabel), m, d.s(46), w-2*m, d.s(16))
	procMoveWindow.Call(uintptr(d.combo), uintptr(m), uintptr(d.s(70)), uintptr(comboW), uintptr(d.s(170)), 1)
	applyX := m + comboW + d.s(10)
	d.move(d.find(idLimitApply), applyX, d.s(70), d.s(84), d.s(26))
	d.move(d.current, applyX, d.s(104), w-applyX-m, d.s(16))

	// Options on completion.
	for i, h := range d.groupC {
		d.move(h, m, d.s(46)+i*d.s(28), w-2*m, d.s(18))
	}

	// The three answers: two right-aligned, one where the eye starts.
	btnW, hideW := d.s(88), d.s(110)
	cancelX := w - m - btnW
	d.move(d.btnHide, m, d.s(btnY), hideW, d.s(btnH))
	d.move(d.btnPause, cancelX-btnW-d.s(8), d.s(btnY), btnW, d.s(btnH))
	d.move(d.btnCancel, cancelX, d.s(btnY), btnW, d.s(btnH))

	// What this dialog draws rather than owns.
	d.bar = rect{int32(m), int32(d.s(barY)), int32(w - m), int32(d.s(barY + barH))}
	d.strip = rect{int32(m), int32(d.s(stripY)), int32(w - m), int32(d.s(stripY + stripH))}
	d.hdr = rect{int32(m), int32(d.s(hdrY)), int32(w - m), int32(d.s(hdrY + hdrH))}
	d.rowsRect = rect{int32(m), int32(d.s(rowsY)), int32(w - m), int32(h - m)}

	d.measureTabs(w)
}

// The drawn regions, in client coordinates. They live here rather than in the
// paint because the paint has to know where to start and the click handler has
// to know where the tabs are before anything has been drawn.
func (d *dlg) move(h windows.Handle, x, y, w, hgt int) {
	if h == 0 {
		return
	}
	procMoveWindow.Call(uintptr(h), uintptr(int32(x)), uintptr(int32(y)), uintptr(int32(w)), uintptr(int32(hgt)), 1)
}

// find returns a child by its control id.
func (d *dlg) find(id int) windows.Handle {
	if d.hwnd == 0 {
		return 0
	}
	// The dialog keeps no index of ids it does not otherwise use, so the two
	// buttons it talks to are found by handle and the rest through this list.
	all := append([]windows.Handle{}, d.groupB...)
	all = append(all, d.groupC...)
	all = append(all, d.groupA...)
	for _, h := range all {
		if h == 0 {
			continue
		}
		got, _, _ := procGetDlgCtrlID.Call(uintptr(h))
		if int32(got) == int32(id) {
			return h
		}
	}
	return 0
}

// measureTabs works out where each tab's box is, so a click can be answered
// without waiting for the first paint and the paint can draw what was agreed.
func (d *dlg) measureTabs(w int) {
	dc, _, _ := procGetDC.Call(uintptr(d.hwnd))
	if dc == 0 {
		return
	}
	defer procReleaseDC.Call(uintptr(d.hwnd), dc)

	pad := d.s(10)
	x := d.s(marginL)
	for i, name := range tabNames {
		procSelectObject.Call(dc, uintptr(th.small))
		var r rect
		r.L, r.R = 0, int32(w)
		r.B = int32(d.s(tabTop + tabH))
		procDrawText.Call(dc, uintptr(unsafe.Pointer(utf16(name))), ^uintptr(0),
			uintptr(unsafe.Pointer(&r)), dtSingleline|dtCalcrect|dtNoPrefix)
		tw := int(r.R - r.L)
		box := rect{int32(x), int32(d.s(tabTop)), int32(x + tw + 2*pad), int32(d.s(tabTop + tabH))}
		if i < len(d.tabBox) {
			d.tabBox[i] = box
		}
		x += tw + 2*pad
	}
}

// onTimer re-reads the queue and says what changed. This is where a finished,
// cancelled or deleted download takes its window down: rather than listening
// for events and keeping two accounts of the same life, the dialog simply
// asks again and acts on the answer in front of it.
func (d *dlg) onTimer() {
	snaps := d.q.Snapshot()
	var found *queue.Snapshot
	for i := range snaps {
		if snaps[i].ID == d.id {
			found = &snaps[i]
			break
		}
	}
	if found == nil {
		debugf("onTimer(%q): no snapshot -> close", d.id)
		d.close()
		return
	}

	switch found.Status {
	case state.StatusCancelled:
		debugf("onTimer(%q): cancelled -> close", d.id)
		d.close()
		return
	case state.StatusDone:
		if d.openFolder && !d.revealed {
			d.revealed = true
			reveal(found.Dest)
		}
		debugf("onTimer(%q): done keepOpen=%v -> %s", d.id, d.keepOpen, "close")
		if !d.keepOpen {
			d.close()
			return
		}
	}
	if !d.ticked {
		d.ticked = true
		debugf("onTimer(%q): first tick status=%s percent=%.1f segs=%d", d.id, found.Status, Percent(*found), len(found.Progress.Segments))
	}
	d.apply(*found)
}

// apply puts one reading of the transfer into the window. Text is only written
// when it differs, so a static is repainted when something happened to it and
// not six times a second merely because a timer is running.
func (d *dlg) apply(s queue.Snapshot) {
	d.status = s.Status
	d.segs = s.Progress.Segments
	d.total = s.TotalSize
	if d.total <= 0 {
		d.total = s.Progress.Total
	}
	d.percent = Percent(s)

	d.setText(idAddress, d.addr, s.URL)
	d.setText(idFileSize, d.sizeH, FormatSize(d.total))
	d.setText(idDownloaded, d.downH, DownloadedText(s.Progress.Done, d.total))
	d.setText(idRate, d.rateH, FormatRate(s.Progress.Speed))
	d.setText(idETA, d.etaH, FormatETA(s.Progress.Done, d.total, s.Progress.Speed))
	d.setText(idResume, d.resumeH, ResumeText(s))

	// A note - an action that did not take - is shown for as long as the
	// state it failed to change stays exactly as it was. The moment the
	// transfer moves on, it has been overtaken and the new state says more.
	status := StatusText(s)
	if d.note != "" && s.Status == d.notedAt {
		status = d.note
	} else {
		d.note = ""
	}
	d.setText(idStatus, d.statusH, status)

	title := TitleBar(s)
	setWindowTitle(d.hwnd, title)

	// The ceiling menu follows the record rather than the other way round, so
	// a limit set from the web table reads back the same here.
	d.setText(idLimitCurrent, d.current, "Currently: "+limitLabel(s.SpeedLimit))
	procSendMessage.Call(uintptr(d.combo), cbSetCurSel, uintptr(indexOfLimit(s.SpeedLimit)), 0)

	// What the middle button does follows what the transfer is doing.
	pause := "Pause"
	cancelVisible := true
	switch s.Status {
	case state.StatusPaused:
		pause = "Resume"
	case state.StatusFailed:
		pause = "Retry"
	case state.StatusDone:
		pause = "Close"
		cancelVisible = false
	}
	d.setText(idBtnPause, d.btnPause, pause)
	if cancelVisible != d.cancelVis {
		d.cancelVis = cancelVisible
		show := uintptr(swShowNA)
		if !cancelVisible {
			show = swHide
		}
		procShowWindow.Call(uintptr(d.btnCancel), show)
	}

	// The status line's colour is the one thing that changes with it: blue
	// while something is happening, red when it stopped for a reason, green
	// for the last word.
	d.statusColor = colorValue
	if s.Status == state.StatusFailed {
		d.statusColor = colorError
	} else if s.Status == state.StatusRunning {
		d.statusColor = colorAccent
	} else if s.Status == state.StatusDone {
		d.statusColor = colorDone
	}

	if d.hwnd != 0 {
		// No erase on purpose: WM_ERASEBKGND is refused and the paint fills
		// the background itself, so a repaint at six hertz cannot flicker.
		procInvalidateRect.Call(uintptr(d.hwnd), 0, 0)
	}
}

// setText writes a control only when what it would say has changed.
func (d *dlg) setText(id int, h windows.Handle, text string) {
	if h == 0 || d.last[id] == text {
		return
	}
	d.last[id] = text
	p, _ := windows.UTF16PtrFromString(text)
	procSetWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(p)))
}

// setWindowTitle changes the title only when it differs, because a title that
// is rewritten sixty times a second is a caption bar flickering for nothing.
func setWindowTitle(hwnd windows.Handle, title string) {
	if hwnd == 0 {
		return
	}
	buf := make([]uint16, 256)
	procGetWindowText.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	current := windows.UTF16ToString(buf)
	if current == title {
		return
	}
	p, _ := windows.UTF16PtrFromString(title)
	procSetWindowText.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
}

// onCommand answers a control. The queue calls are synchronous and made here
// rather than on a goroutine: they take the queue's own lock for as long as an
// HTTP handler takes it, and running them elsewhere would mean a press could
// be followed by a second press before the first had been heard.
func (d *dlg) onCommand(wparam uintptr) {
	id := int(wparam & 0xffff)
	// The high word says what happened to the control. A click is the only
	// word any of these understand - a selection changing in the ceiling menu
	// is answered by the menu itself, and a radio reports its own tick.
	if uint32((wparam>>16)&0xffff) != bnClicked {
		return
	}

	switch id {
	case idBtnHide:
		d.toggleDetails()
	case idBtnPause:
		d.act(d.pauseAction())
	case idBtnCancel:
		d.act(func() error { return d.q.Cancel(d.id) })
	case idLimitApply:
		sel, _, _ := procSendMessage.Call(uintptr(d.combo), cbGetCurSel, 0, 0)
		if int32(sel) >= 0 && int(sel) < len(limitPresets) {
			bps := limitPresets[int(sel)].bps
			d.act(func() error { return d.q.SetLimit(d.id, bps) })
		}
	case idOptClose:
		d.keepOpen, d.openFolder = false, false
	case idOptKeep:
		d.keepOpen, d.openFolder = true, false
	case idOptFolder:
		d.keepOpen, d.openFolder = false, true
	}
}

// pauseAction is what the middle button means for the state the transfer is
// in. One button, four jobs: IDM's own Pause key does the same work.
func (d *dlg) pauseAction() func() error {
	switch d.status {
	case state.StatusPaused:
		return func() error { return d.q.Resume(d.id) }
	case state.StatusFailed:
		return func() error { return d.q.Retry(d.id) }
	case state.StatusDone:
		return func() error { d.close(); return nil }
	default:
		return func() error { return d.q.Pause(d.id) }
	}
}

// act runs one of those and reports a refusal in the status line, which is the
// only place in this window that speaks.
func (d *dlg) act(fn func() error) {
	if fn == nil {
		return
	}
	if err := fn(); err != nil {
		d.note = err.Error()
		d.notedAt = d.status
	}
	d.onTimer()
}

// toggleDetails collapses the map and the connection table - the half of the
// window whose job is explanation rather than control - and grows back to
// exactly the height it had, not to a height somebody guessed at.
func (d *dlg) toggleDetails() {
	d.hidden = !d.hidden
	label := "Hide details"
	if d.hidden {
		label = "Show details"
	}
	d.setText(idBtnHide, d.btnHide, label)

	var rc rect
	procGetClientRect.Call(uintptr(d.hwnd), uintptr(unsafe.Pointer(&rc)))
	h := int(rc.B)
	if d.hidden {
		if h > 0 {
			d.fullH = h
		}
		h = d.s(btnY + btnH + marginL)
	} else if d.fullH > 0 {
		h = d.fullH
	}

	w := int(rc.R)
	outW, outH := outerSize(d.dpi, w, h)
	procSetWindowPos.Call(uintptr(d.hwnd), 0, 0, 0, uintptr(int32(outW)), uintptr(int32(outH)), swpNoMove|swpNoZOrder)
}

// onMouseDown answers a click in the client area, which is where the tabs are.
func (d *dlg) onMouseDown(lparam uintptr) {
	x, y := int32(int16(uint16(lparam&0xffff))), int32(int16(uint16((lparam>>16)&0xffff)))
	for i, box := range d.tabBox {
		if x >= box.L && x < box.R && y >= box.T && y < box.B && i != d.tab {
			d.showTab(i)
			return
		}
	}
}

// ctlColor decides the text colour of a static, which is how this window says
// what matters without ever saying it twice.
func (d *dlg) ctlColor(lparam uintptr) uintptr {
	procSetBkMode.Call(lparam, transparent)
	color := colorValue
	id, _, _ := procGetDlgCtrlID.Call(lparam)
	switch int32(id) {
	case idStatus:
		color = d.statusColor
	case idAddress:
		color = colorValue
	case idLimitLabel, idLimitCurrent, idLblStatus, idLblSize, idLblDown,
		idLblRate, idLblETA, idLblResume:
		color = colorLabel
	}
	procSetTextColor.Call(lparam, uintptr(color))
	return uintptr(th.brushWin)
}

// paint draws what no control owns: the tab strip, the progress bar, the map
// of connections, and the connection list itself.
func (d *dlg) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(d.hwnd), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	if !d.painted {
		d.painted = true
		debugf("paint(%q): first paint, hwnd=%v", d.id, d.hwnd)
	}
	defer procEndPaint.Call(uintptr(d.hwnd), uintptr(unsafe.Pointer(&ps)))

	var rc rect
	procGetClientRect.Call(uintptr(d.hwnd), uintptr(unsafe.Pointer(&rc)))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), uintptr(th.brushBody))

	d.drawTabs(hdc)
	d.drawBar(hdc)
	if !d.hidden {
		d.drawStrip(hdc)
		d.drawTable(hdc, rc)
	}
}

// drawTabs paints the three pages' names across the top. The chosen one is
// dark over an accent rule; the others are grey, which is the whole of the
// difference and enough of it.
func (d *dlg) drawTabs(hdc uintptr) {
	procSetBkMode.Call(hdc, transparent)
	for i, box := range d.tabBox {
		if i >= len(tabNames) {
			break
		}
		procSelectObject.Call(hdc, uintptr(th.small))
		color := colorLabel
		if i == d.tab {
			color = colorValue
		}
		procSetTextColor.Call(hdc, uintptr(color))
		procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16(tabNames[i]))), ^uintptr(0),
			uintptr(unsafe.Pointer(&box)), dtSingleline|dtCenter|dtVCenter|dtNoPrefix)
		if i == d.tab {
			rule := rect{box.L, box.B - int32(d.s(2)), box.R, box.B}
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rule)), uintptr(th.brushBar))
			// The accent is blue wherever it means "now", so the rule under
			// the chosen tab is drawn in the same blue as a live status.
			procSetTextColor.Call(hdc, uintptr(colorAccent))
		}
	}
}

// drawBar is the progress bar: what has arrived, against what has not.
func (d *dlg) drawBar(hdc uintptr) {
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&d.bar)), uintptr(th.brushWalk))
	if d.percent <= 0 {
		return
	}
	full := int32(d.bar.R - d.bar.L)
	filled := int32(float64(full) * d.percent / 100)
	if filled < 1 {
		filled = 1
	}
	if filled > full {
		filled = full
	}
	fill := rect{d.bar.L, d.bar.T, d.bar.L + filled, d.bar.B}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&fill)), uintptr(th.brushBar))
}

// drawStrip is the map under the bar: one block per connection, sitting where
// that connection's bytes sit in the file, filled to how far it has got. This
// is the picture IDM draws, and it is the only view that shows the shape of a
// transfer rather than its arithmetic.
func (d *dlg) drawStrip(hdc uintptr) {
	width := int(d.strip.R - d.strip.L)
	blocks := StripRects(d.segs, d.total, width)
	for _, b := range blocks {
		if b.W <= 0 {
			continue
		}
		x := d.strip.L + int32(b.X)
		// A pixel of air between neighbours, so a join between two finished
		// ranges reads as a join and not as one longer block.
		w := int32(b.W) - 1
		if w < 1 {
			w = 1
		}
		track := rect{x, d.strip.T, x + w, d.strip.B}
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&track)), uintptr(th.brushWalk))
		if b.Filled > 0 {
			filledW := int32(float64(w) * float64(b.Filled) / float64(b.W))
			if filledW < 1 {
				filledW = 1
			}
			if filledW > w {
				filledW = w
			}
			filled := rect{x, d.strip.T, x + filledW, d.strip.B}
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&filled)), uintptr(th.brushBar))
		}
	}
}

// drawTable is the list of connections: which one, how much it is carrying,
// what it is doing. It is drawn rather than built from controls because the
// rows change at the refresh rate and a control per cell would be a hundred
// windows asking to be repainted for numbers nobody read yet.
func (d *dlg) drawTable(hdc uintptr, rc rect) {
	procSetBkMode.Call(hdc, transparent)
	procSelectObject.Call(hdc, uintptr(th.small))
	procSetTextColor.Call(hdc, uintptr(colorLabel))

	m := d.s(marginL)
	colN := d.strip.L
	colDown := d.strip.L + int32(d.s(40))
	colInfo := d.strip.L + int32(d.s(160))

	hdr := d.hdr
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16("N."))), ^uintptr(0), uintptr(unsafe.Pointer(&hdr)), dtSingleline|dtEndEllipsis|dtNoPrefix)
	hdr = d.hdr
	hdr.L, hdr.R = colDown, colInfo-int32(d.s(8))
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16("Downloaded"))), ^uintptr(0), uintptr(unsafe.Pointer(&hdr)), dtSingleline|dtEndEllipsis|dtNoPrefix)
	hdr = d.hdr
	hdr.L, hdr.R = colInfo, d.hdr.R
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16("Info"))), ^uintptr(0), uintptr(unsafe.Pointer(&hdr)), dtSingleline|dtEndEllipsis|dtNoPrefix)

	line := d.s(lineH)
	top := int(d.rowsRect.T)
	bottom := int(d.rowsRect.B)
	for i, seg := range d.segs {
		y := top + i*line
		if y+line > bottom {
			break // the window was dragged shorter than the plan is wide
		}
		row := rect{int32(m), int32(y), rc.R - int32(m), int32(y + line)}

		// A hairline between rows, drawn first so the text sits on it rather
		// than being crossed by it.
		rule := rect{row.L, row.B - 1, row.R, row.B}
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rule)), uintptr(th.brushHair))

		procSetTextColor.Call(hdc, uintptr(colorValue))
		cell := row
		cell.R = colDown - int32(d.s(6))
		cell.L = colN
		procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16(itoa(i+1)))), ^uintptr(0), uintptr(unsafe.Pointer(&cell)), dtSingleline|dtVCenter|dtEndEllipsis|dtNoPrefix)

		cell = row
		cell.L, cell.R = colDown, colInfo-int32(d.s(8))
		procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16(FormatSize(seg.Written)))), ^uintptr(0), uintptr(unsafe.Pointer(&cell)), dtSingleline|dtVCenter|dtEndEllipsis|dtNoPrefix)

		info := ConnectionInfo(seg)
		procSetTextColor.Call(hdc, uintptr(infoColor(seg)))
		cell = row
		cell.L, cell.R = colInfo, row.R
		procDrawText.Call(hdc, uintptr(unsafe.Pointer(utf16(info))), ^uintptr(0), uintptr(unsafe.Pointer(&cell)), dtSingleline|dtVCenter|dtEndEllipsis|dtNoPrefix)
	}
}

// infoColor is the Info column's own colour: blue while a connection is
// working, grey while it waits, green when it is done. The column is read at a
// glance, so it should be legible at a glance.
func infoColor(seg engine.SegState) uint32 {
	switch {
	case seg.Done:
		return colorDone
	case seg.Phase == engine.PhaseQueued:
		return colorLabel
	default:
		return colorAccent
	}
}

// close takes the dialog down without asking anybody. The transfer carries on:
// this window is a report, and a report being closed is not an instruction.
func (d *dlg) close() {
	debugf("close(%q): hwnd=%v status=%s", d.id, d.hwnd, d.status)
	if d.hwnd != 0 {
		procDestroyWindow.Call(uintptr(d.hwnd))
	}
}

// ── thread-owned setup ─────────────────────────────────────────────────────

// registerClass is once per process: the class is what makes a window of this
// kind findable, and a second registration would be refused and ignored.
func registerClass() {
	classOnce.Do(func() {
		name, _ := windows.UTF16PtrFromString(windowClass)
		cursor, _, _ := procLoadCursor.Call(0, idcArrow)
		wc := wndClass{
			WndProc:   wndProcID,
			Instance:  windowsInst(),
			Cursor:    windows.Handle(cursor),
			ClassName: name,
		}
		atom, _, err := procRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
		debugf("registerClass: atom=%d err=%v inst=%v proc=%v", atom, err, wc.Instance, wc.WndProc)
	})
}

// windowsInst is this module, which RegisterClass and CreateWindowEx ask for
// so they know whose window procedure a message belongs to. A nil name asks
// kernel32 for the module the call is running in, which is this process.
func windowsInst() windows.Handle {
	h, _, _ := procGetModuleHandle.Call(0)
	return windows.Handle(h)
}

// makeTheme creates the fonts and brushes every dialog on this thread draws
// with. Two faces are enough: one for what is read, one for what labels it.
func makeTheme(dpi int) {
	th.ui = createFont(dpi, 9, fwNormal)
	th.small = createFont(dpi, 8, fwNormal)
	th.brushBody = createBrush(colorWhite)
	th.brushWalk = createBrush(colorTrack)
	th.brushBar = createBrush(colorAccent)
	th.brushHair = createBrush(colorHair)
	th.brushWin = createBrush(colorWhite)
}

// dropTheme releases them. The thread is the owner, so this is where the
// accounting ends.
func dropTheme() {
	for _, h := range []windows.Handle{th.ui, th.small, th.brushBody, th.brushWalk, th.brushBar, th.brushHair, th.brushWin} {
		if h != 0 {
			procDeleteObject.Call(uintptr(h))
		}
	}
	th = theme{}
}

// createFont makes one Segoe UI face at a point size, for this screen.
func createFont(dpi, points, weight int) windows.Handle {
	name, _ := windows.UTF16PtrFromString("Segoe UI")
	height := -int32(float64(points) * float64(dpi) / 72.0)
	h, _, _ := procCreateFont.Call(
		uintptr(int32(height)),
		0, 0, 0,
		uintptr(weight),
		0, 0, 0,
		defaultCharset, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(name)),
	)
	return windows.Handle(h)
}

func createBrush(c uint32) windows.Handle {
	h, _, _ := procCreateBrush.Call(uintptr(c))
	return windows.Handle(h)
}

// systemDPI is the screen's scale, read the modern way with a fallback for
// Windows too old to have it - the dialog has to be legible on all of them,
// and the fallback costs one call.
func systemDPI() int {
	if procGetDpiForSystem.Find() == nil {
		v, _, _ := procGetDpiForSystem.Call()
		if v > 0 {
			return int(v)
		}
	}
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return 96
	}
	defer procReleaseDC.Call(0, dc)
	v, _, _ := procGetDeviceCaps.Call(dc, logPixelsX)
	if v > 0 {
		return int(v)
	}
	return 96
}

// wndProc is every dialog's window procedure. It is one function for all of
// them, and finds its own dialog in the thread's table - the same thread made
// them all, so no lock is needed to look one up.
func wndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	d := dialogs[hwnd]

	switch msg {
	case wmNcCreate:
		// The window exists from here, which is what lets every later message
		// find the dialog that asked for it.
		debugf("wndProc: WM_NCCREATE hwnd=%v creating=%v", hwnd, creating != nil)
		if creating != nil {
			creating.hwnd = hwnd
			dialogs[hwnd] = creating
		}

	case wmCreate:
		// Controls are made while CreateWindowEx is still on the stack, so a
		// dialog is fully built by the time it is ever shown - nobody sees a
		// window half full of nothing.
		debugf("wndProc: WM_CREATE hwnd=%v d=%v", hwnd, d != nil)
		if d != nil {
			d.build()
		}
		return 0

	case wmEraseBkgnd:
		// Refused: the paint fills the background itself, so nothing has to
		// be painted twice for one frame.
		return 1

	case wmPaint:
		if d != nil {
			d.paint()
			return 0
		}

	case wmCtlColorStatic:
		if d != nil {
			return d.ctlColor(lParam)
		}

	case wmSize:
		if d != nil {
			var rc rect
			procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
			d.layout(int(rc.R), int(rc.B))
			procInvalidateRect.Call(uintptr(hwnd), 0, 0)
			return 0
		}

	case wmTimer:
		if d != nil && wParam == timer {
			d.onTimer()
			return 0
		}

	case wmCommand:
		if d != nil {
			d.onCommand(wParam)
			return 0
		}

	case wmLButtonDown:
		if d != nil {
			d.onMouseDown(lParam)
			return 0
		}

	case wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		debugf("wndProc: WM_DESTROY hwnd=%v d=%v", hwnd, d != nil)
		if d != nil {
			procKillTimer.Call(uintptr(hwnd), timer)
		}
		delete(dialogs, hwnd)
		return 0
	}

	def, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return def
}

// ── small helpers ──────────────────────────────────────────────────────────

func utf16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// limitLabel and indexOfLimit keep the ceiling menu and the record in step
// with each other: whichever side changed last, the other one reads it back.
func limitLabel(bps int64) string {
	for _, p := range limitPresets {
		if p.bps == bps {
			return p.label
		}
	}
	return FormatSize(bps) + "/s"
}

func indexOfLimit(bps int64) int {
	for i, p := range limitPresets {
		if p.bps == bps {
			return i
		}
	}
	return -1 // the record holds a custom ceiling; the menu simply shows none
}

// reveal asks the shell to select the file inside its folder - the same one
// line the API's reveal endpoint runs. It is local rather than shared so this
// package does not import the API, which is about to open this dialog: that
// would close a cycle.
func reveal(dest string) {
	if dest == "" {
		return
	}
	_ = exec.Command("explorer.exe", "/select,"+dest).Start()
}
