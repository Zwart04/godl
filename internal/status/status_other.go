//go:build !windows

package status

import "godl/internal/queue"

// Manager owns the transfer dialogs, which on this platform are nothing.
//
// godl is a Windows program; this file exists so the packages around it can
// still be built and vetted elsewhere, not to promise a dialog anywhere else.
type Manager struct{}

// New prepares a manager that will never show a window.
func New(q *queue.Queue) *Manager { return &Manager{} }

// Open does nothing: there is no window thread, so a queue event would have
// nowhere to go, and saying so by doing nothing is better than failing an
// import.
func (m *Manager) Open(id string) {}

// CloseAll does nothing, for the same reason.
func (m *Manager) CloseAll() {}
