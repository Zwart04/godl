//go:build !windows

package tray

type Action int

const (
	Open Action = iota + 1
	Exit
)

type Manager struct{}

func New() *Manager                    { return &Manager{} }
func (*Manager) Events() <-chan Action { return nil }
func (*Manager) Close()                {}
func (*Manager) WindowHandle() uintptr { return 0 }
func (*Manager) RequestExit()          {}
func (*Manager) RequestOpen()          {}
func (*Manager) IconAdded() bool       { return false }
