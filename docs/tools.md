# Tool Catalog

This document describes the tool surface currently registered by
`internal/tools/register.go`.

## Registration rules

- Tools are registered by group in `internal/tools/*.go` via `AddTool`.
- Mutating tools are skipped when `GITLAB_READ_ONLY_MODE=true`.
- Catalog membership uses additive selection (SW-145). See
  [`docs/configuration.md`](configuration.md#tool-selection-gates) for the
  restricted vs unrestricted matrix and `mcp.json` profiles.
- Legacy opt-in families (unrestricted mode only; still default **off**):
  - `pipeline` — `USE_PIPELINE=true`
  - `milestone` — `USE_MILESTONE=true`
  - `wiki` — `USE_GITLAB_WIKI=true`
- Restricted-mode family flags (also enter restricted mode): `USE_ISSUES`,
  `USE_WORK_ITEMS`, `USE_LABELS`, `USE_DRAFTS`, `USE_WEBHOOKS`, `USE_TIMELINE`.
- `USE_DAILY_TOOLS=true` registers the pinned 41-tool daily census set
  (includes all four search tools below).
- `GITLAB_TOOL_PROFILE=review` registers the closed 52-tool review set (daily +
  discussion get/reply/resolve + six pipeline reads + `get_review_queue` +
  `get_review_snapshot`, no pipeline writes); see
  [`docs/configuration.md`](configuration.md#review-profile-gitlab_tool_profilereview).

## Projects / namespaces / users

- `list_projects`
- `get_project`
- `list_project_members`
- `list_group_projects`
- `list_namespaces`
- `get_namespace`
- `verify_namespace`
- `get_users`

## Repository

- `search_repositories`
- `create_repository`
- `fork_repository`
- `get_file_contents`
- `create_or_update_file`
- `push_files`
- `get_repository_tree`
- `create_branch`
- `list_commits`
- `get_commit`
- `get_commit_diff`
- `get_branch_diffs`

## Merge requests

- `merge_merge_request`
- `create_merge_request`
- `get_merge_request`
- `list_merge_requests`
- `get_review_queue` — the current user's review queue in a group (reviewer and/or
  author roles, deduplicated, per-role page cap with `complete` /
  `truncated_reason`); row `sha` is a list hint, `get_review_snapshot` is the
  head-SHA authority
- `get_review_snapshot` — up to 10 MRs `{project_id, iid, expected_sha?}` in one
  call (`project_id` may be the number from a queue row). Per MR: metadata
  (title, author, reviewers, state, draft, branches, `sha`, `diff_refs`,
  `detailed_merge_status`, `updated_at`) and the `include` sections: `changes`
  (changed files, no patches, `complete` / `truncated_reason` / `next_page`,
  `changes_max_pages` default 3 x 100 files), `approvals` (same shape as
  `get_merge_request_approval_state`) and `discussions` (all pages up to
  `discussions_max_pages`, default 3 x 100, compacted to `{id, resolvable,
  resolved, notes: [{id, author, body, created_at, updated_at, position?,
  system}]}` plus `unresolved_count`, `complete` / `truncated_reason` /
  `next_page`; system notes only with `include_system: true`). After the
  sections the head is read again: `head_changed` (true when it moved meanwhile,
  with `current_sha` = the new head, or when `expected_sha` differs from `sha`;
  `head_recheck_error` if the recheck failed). `include` omitted = all
  implemented sections, `[]` = metadata only (no recheck); `pipeline` is
  reserved and rejected until implemented. A failing MR or section yields an
  `error` entry; the rest of the batch returns
- `update_merge_request`
- `approve_merge_request`
- `unapprove_merge_request`
- `get_merge_request_approval_state`
- `get_merge_request_diffs`
- `list_merge_request_diffs`
- `get_merge_request_conflicts`
- `list_merge_request_changed_files`
- `get_merge_request_file_diff`
- `list_merge_request_versions`
- `get_merge_request_version`

## MR discussions / notes / drafts

- `create_note`
- `create_merge_request_thread`
- `mr_discussions`
- `resolve_merge_request_thread`
- `create_merge_request_note`
- `get_merge_request_note`
- `get_merge_request_notes`
- `update_merge_request_note`
- `delete_merge_request_note`
- `get_merge_request_discussion`
- `create_merge_request_discussion_note`
- `update_merge_request_discussion_note`
- `delete_merge_request_discussion_note`
- `get_draft_note`
- `list_draft_notes`
- `create_draft_note`
- `update_draft_note`
- `delete_draft_note`
- `publish_draft_note`
- `bulk_publish_draft_notes`

## Issues and issue notes

- `list_issues`
- `my_issues`
- `list_project_issues`
- `get_issue`
- `create_issue`
- `update_issue`
- `delete_issue`
- `list_issue_links`
- `get_issue_link`
- `create_issue_link`
- `delete_issue_link`
- `list_issue_discussions`
- `create_issue_note`
- `update_issue_note`

## Labels

- `list_labels`
- `get_label`
- `create_label`
- `update_label`
- `delete_label`

## Pipelines / jobs / deployments / artifacts (gated)

- `list_pipelines`
- `get_pipeline`
- `list_pipeline_jobs`
- `list_pipeline_trigger_jobs`
- `get_pipeline_job`
- `get_pipeline_job_output`
- `create_pipeline`
- `retry_pipeline`
- `cancel_pipeline`
- `play_pipeline_job`
- `retry_pipeline_job`
- `cancel_pipeline_job`
- `list_deployments`
- `get_deployment`
- `list_environments`
- `get_environment`
- `list_job_artifacts`
- `download_job_artifacts`
- `get_job_artifact_file`

## Milestones (gated)

- `list_milestones`
- `get_milestone`
- `create_milestone`
- `edit_milestone`
- `delete_milestone`
- `get_milestone_issue`
- `get_milestone_merge_requests`
- `promote_milestone`
- `get_milestone_burndown_events`

## Releases

- `list_releases`
- `get_release`
- `create_release`
- `update_release`
- `delete_release`
- `create_release_evidence`
- `download_release_asset`

## Wiki (gated)

- `list_wiki_pages`
- `get_wiki_page`
- `create_wiki_page`
- `update_wiki_page`
- `delete_wiki_page`
- `list_group_wiki_pages`
- `get_group_wiki_page`
- `create_group_wiki_page`
- `update_group_wiki_page`
- `delete_group_wiki_page`

## Search / events / markdown / webhooks

Blob / code search (Search API `scope=blobs`; Zoekt when exact code search is
enabled on the instance):

| Tool | Scope |
|---|---|
| `search_code` | Instance-wide |
| `search_project_code` | One project (`project_id`) |
| `search_group_code` | One group (`group_id`) |

Optional inputs on the three `*_code` tools:

| Input | Values / notes |
|---|---|
| `search_type` | `basic` \| `advanced` \| `zoekt` |
| `ref` | Branch or tag name |
| `query` | May embed filters: `filename:*.go`, `path:internal/`, `extension:go` |

Also:

- `search_repositories` (project search; part of the daily set with the three
  blob tools)
- `list_group_iterations`
- `list_events`
- `get_project_events`
- `upload_markdown`
- `download_attachment`
- `list_webhooks` / `list_webhook_events` / `get_webhook_event` (family
  `webhooks`; restricted-mode flag `USE_WEBHOOKS`)

## GraphQL / work items

- `execute_graphql` — arbitrary GraphQL query/mutation (in the daily set)
- `get_work_item` / `list_work_items` / `create_work_item` / `update_work_item`
- `convert_work_item_type` / `list_work_item_statuses` /
  `list_custom_field_definitions` / `move_work_item`
- `list_work_item_notes` / `create_work_item_note`
- `get_timeline_events` / `create_timeline_event` (family `timeline`)

### GraphQL census caveat

REST tool call counts alone understate capability: agents often route through
`execute_graphql` for work items, widgets, and APIs without a dedicated REST
wrapper. A REST family that looks “unused” in a census may still be covered
indirectly via GraphQL. Prefer interpreting usage bands with that in mind when
trimming the daily set.

## Notes

- The definitive source is code registration in `internal/tools`.
- If a tool is added or renamed, update this doc in the same change.
- Selection logic lives in `internal/tools/selection.go`
  (`DailyTools`, `FamilyTools`, `ShouldRegister`).
