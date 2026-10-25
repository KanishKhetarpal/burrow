//go:build !windows

package apps

import "burrow/internal/config"

// Manager is a no-op outside Windows; Burrow only runs on Windows.
type Manager struct{}

// New builds a Manager.
func New() *Manager { return &Manager{} }

// Sync implements Opener.
func (m *Manager) Sync([]config.App) error { return nil }

// Open implements Opener.
func (m *Manager) Open(string) error { return ErrUnknown }
