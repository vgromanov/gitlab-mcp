package tools

import (
	"bytes"
	"log/slog"
	"slices"
	"testing"

	"github.com/vgromanov/gitlab-mcp/internal/config"
)

func TestDailyTools_countAndSearchMembership(t *testing.T) {
	daily := DailyTools()
	if len(daily) != 41 {
		t.Fatalf("DailyTools() len = %d, want 41", len(daily))
	}
	for _, name := range RequiredDailySearchTools {
		if !slices.Contains(daily, name) {
			t.Fatalf("DailyTools missing required search tool %q", name)
		}
	}
	// Pin explicitly so a census trim cannot drop FTS.
	must := []string{"search_code", "search_project_code", "search_group_code", "search_repositories"}
	for _, name := range must {
		if !slices.Contains(daily, name) {
			t.Fatalf("daily set must include %q", name)
		}
	}
}

func TestFamilyTools_knownFamilies(t *testing.T) {
	if got := len(FamilyTools("issues")); got != 14 {
		t.Fatalf("issues family size = %d, want 14", got)
	}
	if got := FamilyTools("nope"); got != nil {
		t.Fatalf("unknown family = %#v, want nil", got)
	}
}

func TestShouldRegister_behaviorMatrix(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		tool       string
		family     string
		want       bool
		restricted bool
	}{
		{
			name:   "unrestricted core always on",
			cfg:    &config.Config{},
			tool:   "list_projects",
			family: "",
			want:   true,
		},
		{
			name:   "unrestricted pipeline off by default",
			cfg:    &config.Config{},
			tool:   "list_pipelines",
			family: "pipeline",
			want:   false,
		},
		{
			name:       "legacy USE_PIPELINE alone registers pipeline (not restricted-only)",
			cfg:        &config.Config{Pipeline: true},
			tool:       "list_pipelines",
			family:     "pipeline",
			want:       true,
			restricted: false,
		},
		{
			name:   "legacy USE_PIPELINE still registers core",
			cfg:    &config.Config{Pipeline: true},
			tool:   "list_projects",
			family: "",
			want:   true,
		},
		{
			name:   "legacy USE_PIPELINE still registers issues (ungated family)",
			cfg:    &config.Config{Pipeline: true},
			tool:   "list_issues",
			family: "issues",
			want:   true,
		},
		{
			name:       "USE_DAILY_TOOLS alone includes search_code",
			cfg:        &config.Config{UseDailyTools: true},
			tool:       "search_code",
			family:     "",
			want:       true,
			restricted: true,
		},
		{
			name:       "USE_DAILY_TOOLS alone excludes issues",
			cfg:        &config.Config{UseDailyTools: true},
			tool:       "list_issues",
			family:     "issues",
			want:       false,
			restricted: true,
		},
		{
			name:       "daily union USE_ISSUES includes list_issues",
			cfg:        &config.Config{UseDailyTools: true, Issues: true},
			tool:       "list_issues",
			family:     "issues",
			want:       true,
			restricted: true,
		},
		{
			name:       "USE_ISSUES alone is restricted-only-issues",
			cfg:        &config.Config{Issues: true},
			tool:       "list_projects",
			family:     "",
			want:       false,
			restricted: true,
		},
		{
			name:       "USE_ISSUES alone includes issues tool",
			cfg:        &config.Config{Issues: true},
			tool:       "get_issue",
			family:     "issues",
			want:       true,
			restricted: true,
		},
		{
			name:       "GITLAB_ENABLED_TOOLS enters restricted and enables named tool",
			cfg:        &config.Config{EnabledTools: []string{"list_labels"}},
			tool:       "list_labels",
			family:     "labels",
			want:       true,
			restricted: true,
		},
		{
			name:   "disable subtracts in unrestricted mode",
			cfg:    &config.Config{DisabledTools: []string{"list_projects"}},
			tool:   "list_projects",
			family: "",
			want:   false,
		},
		{
			name:       "disable subtracts from daily set",
			cfg:        &config.Config{UseDailyTools: true, DisabledTools: []string{"search_code"}},
			tool:       "search_code",
			family:     "",
			want:       false,
			restricted: true,
		},
		{
			name:       "restricted pipeline via family flag",
			cfg:        &config.Config{UseDailyTools: true, Pipeline: true},
			tool:       "list_pipelines",
			family:     "pipeline",
			want:       true,
			restricted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cfg.RestrictedMode() != tc.restricted {
				t.Fatalf("RestrictedMode = %v, want %v", tc.cfg.RestrictedMode(), tc.restricted)
			}
			got := ShouldRegister(tc.cfg, tc.tool, tc.family)
			if got != tc.want {
				t.Fatalf("ShouldRegister(%q,%q) = %v, want %v", tc.tool, tc.family, got, tc.want)
			}
		})
	}
}

func TestShouldRegister_nilConfig(t *testing.T) {
	if ShouldRegister(nil, "list_projects", "") {
		t.Fatal("nil config must not register")
	}
}

func TestNormalizeFamily(t *testing.T) {
	if NormalizeFamily("") != "core" || NormalizeFamily("issues") != "issues" {
		t.Fatal("NormalizeFamily mismatch")
	}
}

func TestWarnUnknownSelectionTools(t *testing.T) {
	resetNotedToolNames()
	noteToolName("list_projects")
	noteToolName("")
	cfg := &config.Config{
		EnabledTools:  []string{"list_projects", "no_such_tool"},
		DisabledTools: []string{"also_missing"},
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	WarnUnknownSelectionTools(nil, log)
	WarnUnknownSelectionTools(cfg, nil)
	WarnUnknownSelectionTools(cfg, log)
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("no_such_tool")) {
		t.Fatalf("expected warning for unknown enable name, got %q", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("also_missing")) {
		t.Fatalf("expected warning for unknown disable name, got %q", out)
	}
	if bytes.Contains(buf.Bytes(), []byte(`tool=list_projects`)) {
		t.Fatalf("should not warn for known tool, got %q", out)
	}
}
