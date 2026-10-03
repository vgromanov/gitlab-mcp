package config

import (
	"flag"
	"io"
	"os"
	"testing"
)

// clearKnownConfigEnv clears every env key read by Load via explicit literal
// names only (no os.Environ enumeration / secret-name filtering).
func clearKnownConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"GITLAB_API_URL",
		"GITLAB_PERSONAL_ACCESS_TOKEN",
		"GITLAB_READ_ONLY_MODE",
		"USE_GITLAB_WIKI",
		"USE_MILESTONE",
		"USE_PIPELINE",
		"USE_DAILY_TOOLS",
		"USE_ISSUES",
		"USE_WORK_ITEMS",
		"USE_LABELS",
		"USE_DRAFTS",
		"USE_WEBHOOKS",
		"USE_TIMELINE",
		"GITLAB_ENABLED_TOOLS",
		"GITLAB_DISABLED_TOOLS",
		"GITLAB_TOOL_PROFILE",
		"STREAMABLE_HTTP",
		"HOST",
		"PORT",
		"GITLAB_PROJECT_ID",
		"GITLAB_ALLOWED_PROJECT_IDS",
		"GITLAB_ALLOWED_GROUP_IDS",
		"GITLAB_CA_CERT_PATH",
		"GITLAB_INSECURE",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"GITLAB_MCP_CURSOR_KEY",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

// withIsolatedFlagCommandLine installs a fresh flag.CommandLine and synthetic
// os.Args for Load(), then restores both. Does not skip on registration panic.
func withIsolatedFlagCommandLine(t *testing.T, args []string) {
	t.Helper()
	oldFS := flag.CommandLine
	oldArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = oldFS
		os.Args = oldArgs
	})
	fs := flag.NewFlagSet("gitlab-mcp-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{"gitlab-mcp"}, args...)
}

func TestNormalizeToolProfile(t *testing.T) {
	if got := NormalizeToolProfile("  Review_Read "); got != "review_read" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeToolProfile("DAILY"); got != "daily" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeToolProfile("  "); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestValidate_toolProfiles(t *testing.T) {
	for _, p := range []string{"", "daily", "review_read", "review_write"} {
		c := &Config{ToolProfile: p}
		if err := c.Validate(); err != nil {
			t.Fatalf("profile %q: %v", p, err)
		}
	}
	c := &Config{ToolProfile: "nope"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected unknown profile error")
	}
	if err := (*Config)(nil).Validate(); err == nil {
		t.Fatal("nil config must fail")
	}
}

func TestApplyToolProfileFlag_visitedEmptyClears(t *testing.T) {
	c := &Config{ToolProfile: "daily"}
	applyToolProfileFlag(c, true, "")
	if c.ToolProfile != "" {
		t.Fatalf("visited empty must clear, got %q", c.ToolProfile)
	}
	c.ToolProfile = "daily"
	applyToolProfileFlag(c, false, "")
	if c.ToolProfile != "daily" {
		t.Fatalf("unvisited must keep env value, got %q", c.ToolProfile)
	}
	applyToolProfileFlag(c, true, " Review_Write ")
	if c.ToolProfile != "review_write" {
		t.Fatalf("got %q", c.ToolProfile)
	}
}

func TestLoad_toolProfileFromFixtureEnv(t *testing.T) {
	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "fake-token-not-live")
	t.Setenv("GITLAB_API_URL", "https://gitlab.example.invalid/api/v4")
	t.Setenv("GITLAB_TOOL_PROFILE", " Daily ")
	withIsolatedFlagCommandLine(t, nil)
	c := Load()
	if c.ToolProfile != "daily" {
		t.Fatalf("ToolProfile=%q want daily", c.ToolProfile)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_toolProfileCLIVisitedEmptyClearsEnv(t *testing.T) {
	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "fake-token-not-live")
	t.Setenv("GITLAB_API_URL", "https://gitlab.example.invalid/api/v4")
	t.Setenv("GITLAB_TOOL_PROFILE", "daily")
	// Visited empty --tool-profile= must clear the fixture env profile.
	withIsolatedFlagCommandLine(t, []string{"-tool-profile="})
	c := Load()
	if c.ToolProfile != "" {
		t.Fatalf("CLI visited empty must clear env profile, got %q", c.ToolProfile)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
