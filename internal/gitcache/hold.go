package gitcache

import (
	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

// ObjectHold is a pinned generation plus its decoded objects for comparison.
// Release must be called. It is not a grant and does not authorize anything.
type ObjectHold struct {
	Result  AcquireResult
	Objects map[plumbing.Hash]pack.Object
	release func() error
}

// Release drops the reader pin. It is safe to call more than once.
// The callback is cleared only after a successful unpin so a failed persist
// can be retried instead of leaking a live pin.
func (h *ObjectHold) Release() error {
	if h == nil || h.release == nil {
		return nil
	}
	if err := h.release(); err != nil {
		return err
	}
	h.release = nil
	return nil
}
