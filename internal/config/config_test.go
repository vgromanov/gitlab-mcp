package config

import (
	"strings"
	"testing"
)

func TestFeatureEnabled(t *testing.T) {
	c := &Config{Wiki: true, Milestone: false, Pipeline: true, Issues: true, Drafts: false}
	if !c.FeatureEnabled("wiki") || c.FeatureEnabled("milestone") || !c.FeatureEnabled("pipeline") {
		t.Fatal("feature flags mismatch")
	}
	if !c.FeatureEnabled("issues") || c.FeatureEnabled("drafts") {
		t.Fatal("new family flags mismatch")
	}
	if !c.FeatureEnabled("unknown_should_default_true") {
		t.Fatal("unknown feature should default true")
	}
}

func TestRestrictedMode(t *testing.T) {
	if (&Config{Pipeline: true}).RestrictedMode() {
		t.Fatal("USE_PIPELINE alone must not enter restricted mode")
	}
	if !(&Config{UseDailyTools: true}).RestrictedMode() {
		t.Fatal("USE_DAILY_TOOLS must enter restricted mode")
	}
	if !(&Config{Issues: true}).RestrictedMode() {
		t.Fatal("USE_ISSUES must enter restricted mode")
	}
	if !(&Config{EnabledTools: []string{"list_projects"}}).RestrictedMode() {
		t.Fatal("GITLAB_ENABLED_TOOLS must enter restricted mode")
	}
	if (&Config{DisabledTools: []string{"list_projects"}}).RestrictedMode() {
		t.Fatal("disable-only must stay unrestricted")
	}
	if !(&Config{ToolProfile: ProfileReview}).RestrictedMode() {
		t.Fatal("GITLAB_TOOL_PROFILE must enter restricted mode")
	}
}

func TestProfileName(t *testing.T) {
	for want, c := range map[string]*Config{
		"review":  {ToolProfile: ProfileReview, UseDailyTools: true},
		"default": {Pipeline: true, DisabledTools: []string{"list_projects"}},
		"daily":   {UseDailyTools: true},
		"custom":  {UseDailyTools: true, Issues: true},
	} {
		if got := c.ProfileName(); got != want {
			t.Fatalf("ProfileName(%+v) = %q, want %q", c, got, want)
		}
	}
	for _, c := range []*Config{{EnabledTools: []string{"get_project"}}, {Drafts: true}, {UseDailyTools: true, EnabledTools: []string{"x"}}} {
		if got := c.ProfileName(); got != "custom" {
			t.Fatalf("ProfileName(%+v) = %q, want custom", c, got)
		}
	}
}

func TestValidToolProfile(t *testing.T) {
	for profile, want := range map[string]bool{"": true, ProfileReview: true, "daily": false, "bogus": false} {
		if got := (&Config{ToolProfile: profile}).ValidToolProfile(); got != want {
			t.Fatalf("ValidToolProfile(%q) = %v, want %v", profile, got, want)
		}
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("MCP_TEST_BOOL", "true")
	if !envBool("MCP_TEST_BOOL", false) {
		t.Fatal("expected true")
	}
	t.Setenv("MCP_TEST_BOOL", "garbage")
	if !envBool("MCP_TEST_BOOL", true) {
		t.Fatal("invalid should return default true")
	}
}

func TestEnvString(t *testing.T) {
	t.Setenv("MCP_TEST_STR", " hello ")
	if envString("MCP_TEST_STR", "def") != "hello" {
		t.Fatalf("got %q", envString("MCP_TEST_STR", "def"))
	}
	if envString("MCP_TEST_STR_MISSING", "def") != "def" {
		t.Fatal("default")
	}
}

func TestAllowedProjectIDs_split(t *testing.T) {
	raw := " 1 , foo/bar , "
	var ids []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			ids = append(ids, p)
		}
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "foo/bar" {
		t.Fatalf("got %#v", ids)
	}
}
