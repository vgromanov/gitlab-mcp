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
func (h *ObjectHold) Release() error {
	if h == nil || h.release == nil {
		return nil
	}
	err := h.release()
	h.release = nil
	return err
}
