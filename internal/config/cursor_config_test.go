package config

import (
	"strings"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
)

func TestValidate_missingCursorKeyLegacyOK(t *testing.T) {
	clearKnownConfigEnv(t)
	withIsolatedFlagCommandLine(t, []string{"-token=fake-token-not-live", "-api-url=https://gitlab.example.invalid/api/v4"})
	c := Load()
	if len(c.CursorKey) != 0 {
		t.Fatalf("expected empty cursor key, got len=%d", len(c.CursorKey))
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("missing key must allow legacy startup: %v", err)
	}
	if c.CursorSigningEnabled() {
		t.Fatal("signing must be disabled without key")
	}
	if c.CursorTTL() != cursor.DefaultTTL {
		t.Fatalf("ttl=%v", c.CursorTTL())
	}
}

func TestValidate_presentInvalidCursorKeyNoEcho(t *testing.T) {
	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_MCP_CURSOR_KEY", "too-short-secret")
	withIsolatedFlagCommandLine(t, []string{"-token=fake-token-not-live", "-api-url=https://gitlab.example.invalid/api/v4"})
	c := Load()
	err := c.Validate()
	if err == nil {
		t.Fatal("expected invalid key error")
	}
	if !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
		t.Fatalf("unexpected err: %v", err)
	}
	if strings.Contains(err.Error(), "too-short-secret") {
		t.Fatal("key material must not be echoed")
	}
}

func TestLoad_validCursorKey(t *testing.T) {
	clearKnownConfigEnv(t)
	raw := strings.Repeat("k", cursor.MinKeyBytes)
	t.Setenv("GITLAB_MCP_CURSOR_KEY", raw)
	withIsolatedFlagCommandLine(t, []string{"-token=fake-token-not-live", "-api-url=https://gitlab.example.invalid/api/v4"})
	c := Load()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.CursorSigningEnabled() {
		t.Fatal("expected signing enabled")
	}
	if string(c.CursorKey) != raw {
		t.Fatal("cursor key mismatch (values compared in test only)")
	}
}
