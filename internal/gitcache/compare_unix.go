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
// Callers must invoke the returned cleanup to release the reservation.
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
	reserved := uint64(size)
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return "", nil, ErrClosed
	}
	if err := m.reserveCompareScratchLocked(reserved); err != nil {
		m.mu.Unlock()
		return "", nil, err
	}
	rootName := m.root.Name()
	m.mu.Unlock()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		m.releaseCompareScratch(reserved)
		return "", nil, err
	}
	dir := filepath.Join(rootName, compareDirName, hex.EncodeToString(buf[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.releaseCompareScratch(reserved)
		return "", nil, err
	}
	return dir, func() error {
		err := os.RemoveAll(dir)
		if err == nil {
			m.releaseCompareScratch(reserved)
		}
		return err
	}, nil
}
