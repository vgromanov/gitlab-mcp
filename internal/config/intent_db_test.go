package config

import (
	"path/filepath"
	"testing"
)

func TestLoad_intentDB(t *testing.T) {
	clearKnownConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "intent.db")
	t.Setenv("GITLAB_MCP_INTENT_DB", "  "+path+"  ")
	withIsolatedFlagCommandLine(t, []string{"-token=fake-token-not-live", "-api-url=https://gitlab.example.invalid/api/v4"})
	c := Load()
	if c.IntentDB != path {
		t.Fatalf("IntentDB=%q", c.IntentDB)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_intentDBRelative(t *testing.T) {
	c := &Config{IntentDB: "relative/intent.db"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected relative path to fail")
	}
	if err.Error() != "GITLAB_MCP_INTENT_DB must be an absolute path" {
		t.Fatalf("err %v", err)
	}
}

func TestValidate_reviewWriteWithoutIntentDB(t *testing.T) {
	c := &Config{ToolProfile: "review_write"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
