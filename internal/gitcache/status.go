package gitcache

import "errors"

// Fixed public status categories. Diagnostics never include credentials,
// protocol error graphs, or upstream response bodies.
const (
	StatusOK          = "ok"
	StatusBusy        = "cache_busy"
	StatusQuota       = "quota"
	StatusCorrupt     = "corrupt"
	StatusUnsupported = "unsupported"
	StatusClosed      = "closed"
	StatusPinned      = "pinned"
	StatusLimit       = "limit"
	StatusDenied      = "denied"
	StatusUnavailable = "unavailable"
	StatusPartial     = "partial"
	StatusCanceled    = "canceled"
)

var (
	ErrOverflow    = errors.New("gitcache: overflow")
	ErrQuota       = errors.New("gitcache: quota")
	ErrBusy        = errors.New("gitcache: cache_busy")
	ErrCorrupt     = errors.New("gitcache: corrupt")
	ErrUnsupported = errors.New("gitcache: unsupported")
	ErrClosed      = errors.New("gitcache: closed")
	ErrPinned      = errors.New("gitcache: pinned")
	ErrLimit       = errors.New("gitcache: limit")
	ErrPath        = errors.New("gitcache: path")
	ErrState       = errors.New("gitcache: state")
	ErrDenied      = errors.New("gitcache: denied")
	ErrUnavailable = errors.New("gitcache: unavailable")
	ErrPartial     = errors.New("gitcache: partial")
	ErrDisabled    = errors.New("gitcache: disabled")
	ErrPlatform    = errors.New("gitcache: platform unsupported")
	ErrCanceled    = errors.New("gitcache: canceled")
	ErrIntegrity   = errors.New("gitcache: integrity")
	ErrAuthz       = errors.New("gitcache: authorization")
	ErrCapability  = errors.New("gitcache: capability")
	ErrScheme      = errors.New("gitcache: scheme")
)
