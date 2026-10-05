package config

import (
	"strings"
	"testing"
)

func TestGitCacheValidateRequiresRootWhenEnabled(t *testing.T) {
	c := &Config{GitCacheEnabled: true}
	if err := c.Validate(); err == nil {
		t.Fatal("expected root required")
	}
	c.GitCacheRoot = "/tmp/cache"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.GitCacheInsecure = true
	c.GitCacheAllowedInsecureHost = ""
	if err := c.Validate(); err == nil {
		t.Fatal("expected insecure host required")
	}
}

func TestGitCacheQuotaRejectsMalformedAndNegative(t *testing.T) {
	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES", "nope")
	withIsolatedFlagCommandLine(t, nil)
	c := Load()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_GIT_CACHE_QUOTA_BYTES") {
		t.Fatalf("malformed env: %v", err)
	}
	if c.GitCacheQuotaBytes != 0 {
		t.Fatalf("malformed stored %d", c.GitCacheQuotaBytes)
	}

	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES", "-1")
	withIsolatedFlagCommandLine(t, nil)
	c = Load()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_GIT_CACHE_QUOTA_BYTES") {
		t.Fatalf("negative env: %v", err)
	}

	clearKnownConfigEnv(t)
	withIsolatedFlagCommandLine(t, []string{"-git-cache-quota-bytes=-5"})
	c = Load()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--git-cache-quota-bytes") {
		t.Fatalf("negative flag: %v", err)
	}
	if c.GitCacheEnabled {
		t.Fatal("negative quota must fail even while the cache stays disabled")
	}

	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES", "nope")
	withIsolatedFlagCommandLine(t, []string{"-git-cache-quota-bytes=0"})
	c = Load()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.GitCacheQuotaBytes != 0 {
		t.Fatalf("explicit zero stored %d", c.GitCacheQuotaBytes)
	}

	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES", "4096")
	withIsolatedFlagCommandLine(t, nil)
	c = Load()
	if err := c.Validate(); err != nil || c.GitCacheQuotaBytes != 4096 {
		t.Fatalf("positive env: %d %v", c.GitCacheQuotaBytes, err)
	}

	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES", "nope")
	withIsolatedFlagCommandLine(t, []string{"-git-cache-quota-bytes=100"})
	c = Load()
	if err := c.Validate(); err != nil || c.GitCacheQuotaBytes != 100 {
		t.Fatalf("flag override: %d %v", c.GitCacheQuotaBytes, err)
	}
}

func TestGitCacheDefaultDisabled(t *testing.T) {
	c := &Config{}
	if c.GitCacheEnabled {
		t.Fatal("enabled by default")
	}
}
