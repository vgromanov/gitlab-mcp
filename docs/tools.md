# Tool Catalog

This document describes the tool surface currently registered by
`internal/tools/register.go`.

## Registration rules

- Tools are registered by group in `internal/tools/*.go`.
- Mutating tools are disabled when `GITLAB_READ_ONLY_MODE=true`.
- Some groups are feature-gated:
  - `pipeline` (enable with `USE_PIPELINE=true`)
  - `milestone` (enable with `USE_MILESTONE=true`)
  - `wiki` (enable with `USE_GITLAB_WIKI=true`)

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

- `search_code`
- `search_project_code`
- `search_group_code`
- `list_group_iterations`
- `list_events`
- `get_project_events`
- `upload_markdown`
- `download_attachment`
- `list_webhooks`
- `list_webhook_events`
- `get_webhook_event`

## GraphQL / work items

- `execute_graphql`
- `get_work_item`
- `list_work_items`
- `create_work_item`
- `update_work_item`
- `convert_work_item_type`
- `list_work_item_statuses`
- `list_custom_field_definitions`
- `move_work_item`
- `list_work_item_notes`
- `create_work_item_note`
- `get_timeline_events`
- `create_timeline_event`

## Notes

- The definitive source is code registration in `internal/tools`.
- If a tool is added or renamed, update this doc in the same change.
