package gitcache

import (
	"crypto/tls"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
)

// AcquireIntent is the only caller-facing acquisition input. It identifies the
// MR to authorize; it does not grant access. Immutable Expected* fields, when
// set, must match a fresh API DiffRefs observation or the call fails closed
// (moved/stale). Caller-populated Grant structs are not accepted and cannot
// authorize anything.
type AcquireIntent struct {
	preparedTLS          *tls.Config
	trustFP              string
	sourceSSH, targetSSH listx.Options
	trustTransitions     []sshtrust.Transition

	ProjectID string
	MRIID     int

	// Expected immutable roots from a prior authorized binding. Zero means
	// "bind whatever DiffRefs the fresh API read returns".
	ExpectedHead      plumbing.Hash
	ExpectedBase      plumbing.Hash
	ExpectedStart     plumbing.Hash
	ExpectedMRVersion string

	Transport           string // "https" or "ssh"
	CAPath              string
	Insecure            bool
	AllowedInsecureHost string
	Token               string // in-memory only; never persisted
	Depth               int    // must be 1 or 2; never 0 / full-history

	// AllowLoopback permits 127.0.0.1/localhost fetch URLs for hermetic tests.
	// Production wiring must leave this false; it does not authorize content.
	AllowLoopback bool

	// SSH carries native SSH listing/fetch options (config/identity/trust).
	// Production wiring leaves the zero value and relies on process SSH config.
	SSH listx.Options
}

// AcquireResult summarizes one acquisition without secrets or file contents.
type AcquireResult struct {
	Warm         bool
	Objects      int
	PackBytes    int
	HeapAlloc    uint64
	Elapsed      time.Duration
	GenerationID string
	Grant        Grant // authoritative binding used for this result
}
