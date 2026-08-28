package tools

import (
	"log/slog"
	"slices"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
)

// RequiredDailySearchTools must always appear in the USE_DAILY_TOOLS set.
// Census marks them core; pin here so a later trim cannot drop FTS by accident.
var RequiredDailySearchTools = []string{
	"search_code",
	"search_project_code",
	"search_group_code",
	"search_repositories",
}

// dailyTools is the Aug-2026 census set (band core|rare), excluding Cursor mcp_auth.
// Source: canvas go-gitlab-tool-usage.canvas.tsx TOOLS; vault note dated 2026-08-20.
var dailyTools = []string{
	"execute_graphql",
	"create_merge_request",
	"merge_merge_request",
	"search_repositories",
	"get_file_contents",
	"get_project",
	"get_merge_request",
	"list_group_projects",
	"list_project_members",
	"search_code",
	"list_merge_requests",
	"list_projects",
	"get_namespace",
	"get_repository_tree",
	"search_group_code",
	"search_project_code",
	"update_merge_request",
	"create_repository",
	"get_users",
	"get_commit",
	"get_merge_request_approval_state",
	"create_merge_request_note",
	"get_project_events",
	"list_commits",
	"mr_discussions",
	"get_merge_request_notes",
	"get_merge_request_conflicts",
	"create_branch",
	"create_release",
	"list_namespaces",
	"list_releases",
	"approve_merge_request",
	"get_merge_request_file_diff",
	"list_merge_request_changed_files",
	"create_merge_request_thread",
	"get_commit_diff",
	"get_merge_request_diffs",
	"get_merge_request_version",
	"list_merge_request_versions",
	"create_or_update_file",
	"push_files",
}

// familyTools maps gated family ids to their tool names (mirrors AddTool tags).
var familyTools = map[string][]string{
	"issues": {
		"list_issues", "my_issues", "list_project_issues", "get_issue",
		"create_issue", "update_issue", "delete_issue",
		"list_issue_links", "get_issue_link", "create_issue_link", "delete_issue_link",
		"list_issue_discussions", "create_issue_note", "update_issue_note",
	},
	"work_items": {
		"get_work_item", "list_work_items", "create_work_item", "update_work_item",
		"convert_work_item_type", "list_work_item_statuses", "list_custom_field_definitions",
		"move_work_item", "list_work_item_notes", "create_work_item_note",
	},
	"labels": {
		"list_labels", "get_label", "create_label", "update_label", "delete_label",
	},
	"drafts": {
		"get_draft_note", "list_draft_notes", "create_draft_note", "update_draft_note",
		"delete_draft_note", "publish_draft_note", "bulk_publish_draft_notes",
	},
	"webhooks": {
		"list_webhooks", "list_webhook_events", "get_webhook_event",
	},
	"timeline": {
		"get_timeline_events", "create_timeline_event",
	},
	"pipeline": {
		"list_pipelines", "get_pipeline", "list_pipeline_jobs", "list_pipeline_trigger_jobs",
		"get_pipeline_job", "get_pipeline_job_output", "create_pipeline", "retry_pipeline",
		"cancel_pipeline", "play_pipeline_job", "retry_pipeline_job", "cancel_pipeline_job",
		"list_job_artifacts", "download_job_artifacts", "get_job_artifact_file",
		"list_deployments", "get_deployment", "list_environments", "get_environment",
	},
	"milestone": {
		"list_milestones", "get_milestone", "create_milestone", "edit_milestone",
		"delete_milestone", "get_milestone_issue", "get_milestone_merge_requests",
		"promote_milestone", "get_milestone_burndown_events",
	},
	"wiki": {
		"list_wiki_pages", "get_wiki_page", "create_wiki_page", "update_wiki_page",
		"delete_wiki_page", "list_group_wiki_pages", "get_group_wiki_page",
		"create_group_wiki_page", "update_group_wiki_page", "delete_group_wiki_page",
	},
}

// legacyGatedFamilies stay off by default in unrestricted mode (today's behavior).
var legacyGatedFamilies = map[string]struct{}{
	"pipeline":  {},
	"milestone": {},
	"wiki":      {},
}

// notedToolNames records every tool that attempted registration (for unknown-name warnings).
var notedToolNames = map[string]struct{}{}

func noteToolName(name string) {
	if name == "" {
		return
	}
	notedToolNames[name] = struct{}{}
}

func resetNotedToolNames() {
	notedToolNames = map[string]struct{}{}
}

// DailyTools returns the pinned USE_DAILY_TOOLS catalog (census core|rare).
func DailyTools() []string {
	return slices.Clone(dailyTools)
}

// FamilyTools returns tool names tagged with the given family id.
func FamilyTools(name string) []string {
	if tools, ok := familyTools[name]; ok {
		return slices.Clone(tools)
	}
	return nil
}

// NormalizeFamily maps empty family tags to "core".
func NormalizeFamily(family string) string {
	if family == "" {
		return "core"
	}
	return family
}

// ShouldRegister reports whether toolName should be added to the MCP catalog.
//
// Gate order (after read-only): restricted vs legacy → enable union → disable subtract.
func ShouldRegister(cfg *config.Config, toolName, family string) bool {
	if cfg == nil {
		return false
	}
	family = NormalizeFamily(family)

	if toolNameInList(cfg.DisabledTools, toolName) {
		return false
	}

	if !cfg.RestrictedMode() {
		if _, gated := legacyGatedFamilies[family]; gated {
			return cfg.FeatureEnabled(family)
		}
		return true
	}

	// Restricted: empty base, then union of enable sources.
	if cfg.UseDailyTools && toolNameInList(dailyTools, toolName) {
		return true
	}
	if familyFlagEnabled(cfg, family) {
		return true
	}
	if toolNameInList(cfg.EnabledTools, toolName) {
		return true
	}
	return false
}

func familyFlagEnabled(cfg *config.Config, family string) bool {
	switch family {
	case "issues":
		return cfg.Issues
	case "work_items":
		return cfg.WorkItems
	case "labels":
		return cfg.Labels
	case "drafts":
		return cfg.Drafts
	case "webhooks":
		return cfg.Webhooks
	case "timeline":
		return cfg.Timeline
	case "pipeline":
		return cfg.Pipeline
	case "milestone":
		return cfg.Milestone
	case "wiki":
		return cfg.Wiki
	default:
		return false
	}
}

func toolNameInList(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// WarnUnknownSelectionTools logs enable/disable names that never matched a registered tool.
func WarnUnknownSelectionTools(cfg *config.Config, log *slog.Logger) {
	if cfg == nil || log == nil {
		return
	}
	warnUnknown := func(kind string, names []string) {
		for _, n := range names {
			if _, ok := notedToolNames[n]; !ok {
				log.Warn("unknown tool name in selection list; ignored", "list", kind, "tool", n)
			}
		}
	}
	warnUnknown("GITLAB_ENABLED_TOOLS", cfg.EnabledTools)
	warnUnknown("GITLAB_DISABLED_TOOLS", cfg.DisabledTools)
}
