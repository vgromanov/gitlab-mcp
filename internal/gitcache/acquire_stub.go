//go:build !linux && !darwin

package gitcache

import "context"

// Acquire is unsupported on this platform.
func (m *Manager) Acquire(context.Context, AcquireIntent, Authorizer) (*AcquireResult, error) {
	return nil, ErrPlatform
}
