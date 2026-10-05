//go:build !linux && !darwin

package gitcache

import "context"

// Manager is unavailable outside Linux and Darwin.
type Manager struct{}

// OpenManager refuses to create a cache root on an unsupported platform.
func OpenManager(string, int64) (*Manager, error) { return nil, ErrPlatform }

// Close implements the shutdown surface.
func (m *Manager) Close(context.Context) error { return ErrPlatform }
