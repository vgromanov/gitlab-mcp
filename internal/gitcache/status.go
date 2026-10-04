package gitcache

import "errors"

// Static statuses. Diagnostics never include upstream text or secrets.
const (
	StatusOK           = "ok"
	StatusBusy         = "cache_busy"
	StatusQuota        = "quota"
	StatusCorrupt      = "corrupt"
	StatusUnsupported  = "unsupported"
	StatusClosed       = "closed"
	StatusPinned       = "pinned"
	StatusNotQuiescent = "not_quiescent"
	StatusLimit        = "limit"
	StatusAudit        = "audit"
)

var (
	ErrOverflow     = errors.New("gitcache: overflow")
	ErrQuota        = errors.New("gitcache: quota")
	ErrBusy         = errors.New("gitcache: cache_busy")
	ErrCorrupt      = errors.New("gitcache: corrupt")
	ErrUnsupported  = errors.New("gitcache: unsupported")
	ErrClosed       = errors.New("gitcache: closed")
	ErrPinned       = errors.New("gitcache: pinned")
	ErrNotQuiescent = errors.New("gitcache: not_quiescent")
	ErrLimit        = errors.New("gitcache: limit")
	ErrPath         = errors.New("gitcache: path")
	ErrState        = errors.New("gitcache: state")
	ErrAudit        = errors.New("gitcache: audit")
	ErrAbsent       = errors.New("gitcache: absent")
)

// AllowEnv is the only environment a helper process receives.
func AllowEnv() []string {
	return []string{
		"HOME=/nonexistent",
		"XDG_CONFIG_HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"GOTRACEBACK=none",
	}
}

// GitEnv is AllowEnv plus the fixed Git switches for an audited role.
// It does not include PATH, proxy, or credential variables.
func GitEnv() []string {
	return append(AllowEnv(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_DIR=.",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1",
	)
}
