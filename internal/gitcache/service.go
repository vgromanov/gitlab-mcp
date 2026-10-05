package gitcache

import (
	"context"
	"errors"
	"sync"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// Service owns optional manager lifetime for the MCP server process.
// When disabled, all methods are no-ops / ErrDisabled and open no filesystem.
type Service struct {
	mu      sync.Mutex
	closeMu sync.Mutex
	enabled bool
	mgr     *Manager
	domain  AuthDomain
	cfg     ServiceConfig
}

// ServiceConfig is cache-only startup configuration.
type ServiceConfig struct {
	Token               string // existing API credential, memory only
	Enabled             bool
	Root                string
	QuotaBytes          int64
	CAPath              string
	Insecure            bool
	AllowedInsecureHost string
}

// OpenService returns a disabled service when cfg.Enabled is false.
// On unsupported platforms with Enabled set, it returns ErrPlatform.
func OpenService(cfg ServiceConfig) (*Service, error) {
	s := &Service{enabled: cfg.Enabled, cfg: cfg}
	if !cfg.Enabled {
		return s, nil
	}
	quota := cfg.QuotaBytes
	if quota <= 0 {
		quota = bounds.DefaultQuotaBytes
	}
	mgr, err := OpenManager(cfg.Root, quota)
	if err != nil {
		return nil, err
	}
	s.mgr = mgr
	return s, nil
}

// Enabled reports whether the cache is active.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled && s.mgr != nil
}

// Domain returns the process-local auth domain key binder.
func (s *Service) Domain() *AuthDomain {
	if s == nil {
		return nil
	}
	return &s.domain
}

// Manager returns the underlying manager or nil when disabled.
func (s *Service) Manager() *Manager {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return nil
	}
	return s.mgr
}

// Acquire is the single cache content path: ResolveGrant via auth, then warm
// lookup / cold fetch / publication. Caller Grant fields are never accepted.
func (s *Service) Acquire(ctx context.Context, intent AcquireIntent, auth Authorizer) (*AcquireResult, error) {
	if s == nil {
		return nil, ErrDisabled
	}
	s.mu.Lock()
	if !s.enabled || s.mgr == nil {
		s.mu.Unlock()
		return nil, ErrDisabled
	}
	mgr := s.mgr
	cfg := s.cfg
	s.mu.Unlock()
	if auth == nil {
		return nil, ErrAuthz
	}
	if cfg.Token != "" {
		if intent.Token != "" && intent.Token != cfg.Token {
			return nil, ErrAuthz
		}
		intent.Token = cfg.Token
	}
	intent = applyServiceTLS(intent, cfg)
	return mgr.Acquire(ctx, intent, auth)
}

// Hold re-authorizes and returns pinned generation objects for comparison.
// A warm generation still goes through ResolveGrant. The caller must Release.
func (s *Service) Hold(ctx context.Context, intent AcquireIntent, auth Authorizer) (*ObjectHold, error) {
	if s == nil {
		return nil, ErrDisabled
	}
	s.mu.Lock()
	if !s.enabled || s.mgr == nil {
		s.mu.Unlock()
		return nil, ErrDisabled
	}
	mgr := s.mgr
	cfg := s.cfg
	s.mu.Unlock()
	if auth == nil {
		return nil, ErrAuthz
	}
	if cfg.Token != "" {
		if intent.Token != "" && intent.Token != cfg.Token {
			return nil, ErrAuthz
		}
		intent.Token = cfg.Token
	}
	intent = applyServiceTLS(intent, cfg)
	return mgr.Hold(ctx, intent, auth)
}

func applyServiceTLS(intent AcquireIntent, cfg ServiceConfig) AcquireIntent {
	// Startup configuration is authoritative for the service. Direct Manager
	// acquisition has its own explicit per-operation trust input for hermetic use.
	intent.CAPath = cfg.CAPath
	intent.Insecure = cfg.Insecure
	intent.AllowedInsecureHost = cfg.AllowedInsecureHost
	return intent
}

// Close shuts down the manager when enabled. Incomplete moves (pinned /
// canceled join) retain the manager handle for a later retry.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	mgr := s.mgr
	s.mu.Unlock()
	if mgr == nil {
		return nil
	}
	err := mgr.Close(ctx)
	if errors.Is(err, ErrPinned) || errors.Is(err, ErrCanceled) {
		return err
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mgr == mgr {
		s.mgr = nil
		s.enabled = false
	}
	return nil
}
