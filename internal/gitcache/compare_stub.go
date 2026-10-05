//go:build !linux && !darwin

package gitcache

import "context"

// OpenCompareDir is unsupported on this platform.
func (m *Manager) OpenCompareDir(context.Context, int64) (string, func() error, error) {
	return "", nil, ErrPlatform
}
