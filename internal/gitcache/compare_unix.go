//go:build linux || darwin

package gitcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// OpenCompareDir reserves comparison scratch under the private cache root.
// Callers must invoke the returned cleanup.
func (m *Manager) OpenCompareDir(ctx context.Context, size int64) (string, func() error, error) {
	if m == nil {
		return "", nil, ErrDisabled
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if size < 0 {
		size = 0
	}
	if size > bounds.MaxScratchBytes {
		return "", nil, ErrLimit
	}
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return "", nil, ErrClosed
	}
	charged, err := chargedTotal(m.slots)
	if err != nil {
		m.mu.Unlock()
		return "", nil, err
	}
	need := charged + uint64(size) + uint64(bounds.BrootBytes)
	if need < charged || need > m.quota {
		m.mu.Unlock()
		return "", nil, ErrQuota
	}
	rootName := m.root.Name()
	m.mu.Unlock()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(rootName, compareDirName, hex.EncodeToString(buf[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	return dir, func() error { return os.RemoveAll(dir) }, nil
}
