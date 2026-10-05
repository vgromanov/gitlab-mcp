# Tool Catalog

This document describes the tool surface currently registered by
`internal/tools/register.go`.

## Registration rules

- Tools are registered by group in `internal/tools/*.go` via `AddTool`.
- Mutating tools are skipped when `GITLAB_READ_ONLY_MODE=true`.
- Catalog membership uses additive selection (SW-145) plus optional
  `GITLAB_TOOL_PROFILE` ceilings. See
  [`docs/configuration.md`](configuration.md#tool-selection-gates) for the
  restricted vs unrestricted matrix, named profiles (`daily` / `review_read` /
  `review_write`), and `mcp.json` examples. `review_write` currently registers
  the same reads as `review_read` — no guarded writes are available yet.
- Legacy opt-in families (unrestricted mode only; still default **off**):
  - `pipeline` — `USE_PIPELINE=true`
  - `milestone` — `USE_MILESTONE=true`
  - `wiki` — `USE_GITLAB_WIKI=true`
- Restricted-mode family flags (also enter restricted mode): `USE_ISSUES`,
  `USE_WORK_ITEMS`, `USE_LABELS`, `USE_DRAFTS`, `USE_WEBHOOKS`, `USE_TIMELINE`.
- `USE_DAILY_TOOLS=true` registers the pinned 41-tool daily census set
  (includes all four search tools below).

## Projects / namespaces / users

- `list_projects`
- `get_project` — **allowlisted projection only** (intentional reduction / migration
  from the previous full SDK object). Retained fields: `id`, `name`, `path`,
  `path_with_namespace`, `default_branch`, `visibility`, `archived`; optional
  `web_url` when it is a safe absolute HTTP(S) project page URL (usable host,
  no userinfo, no query, no fragment, no control characters — otherwise
  omitted); optional `namespace` with only `id`, `name`, `path`, `full_path`,
  `kind` (omitted when null). Callers must not expect raw SDK fields
  (`runners_token`, `import_url`, `http_url_to_repo`, nested namespace extras,
  or future SDK keys). Rollback keeps this safe projection — it does not
  restore the full SDK response. Backend failures on this path return fixed
  safe error codes/messages (no raw SDK objects, response bodies, or
  credentials).
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
- `batch_get_file_contents` — bounded batch read of up to 20 paths at one full
  40-hex commit SHA (independent per-path ranges/errors; 1 MiB returned raw
  aggregate default; streaming/ranged raw provider; readmeta section envelope).
  Does not change legacy `get_file_contents`.
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
- `list_merge_requests` — list globally or by `project_id` / `group_id` (mutually exclusive). Optional filters: `state`, positive `author_id` / `reviewer_id`, explicit `scope` (`created_by_me|assigned_to_me|reviews_for_me|all`; omit for legacy GitLab default — do not invent `scope=all`), `updated_after` / `updated_before` (strict RFC3339 / RFC3339Nano: 2-digit hour, `.` fractions only, zone `Z` or `±HH:MM` with legal HH/MM; **at most 9 fractional digits / nanosecond precision** — longer fractions are rejected, never silently truncated; accepted instants are forwarded losslessly), `order_by` (`created_at|updated_at|label_priority|priority|milestone_due|popularity|title`), `sort` (`asc|desc`), plus pagination. Reviewer discovery: pass `scope=all` with `reviewer_id`. Group lists forward `author_id` (no longer dropped).
- `get_merge_request_review_queue` — nonmutating canonical group review-queue aggregate for requested membership kinds (`reviewer` / `ongoing` / `authored`). Two-phase discover→emit with signed `queue_cont` continuation. `until` is `min(caller updated_before, initial UTC)` serialized with RFC3339Nano and is not renewed. Owner, group policy, exact MR, and source/downstream authorization complete before any candidate key, timestamp, head, or membership bit is placed in a returned cursor; a budget stop keeps only progress indexes. Requested `project_ids` (numeric or path) filter reviewer, authored, and ongoing before MR or discussion reads. Provider pages accept only exhaustion or exactly the next page. A note scan that stops inside a discussion is terminal (`budget_items`, `partial`, `membership_incomplete`) with no cursor, because rq2 cannot store a note index. Output keeps aggregate `section`, per-kind `sections`, and `queue_counts` (`confirmed_candidates`, `returned_items`, `known_terminal_omitted`, nullable `unobserved_membership_count`). The whole JSON document is capped at 256 KiB. Review profiles only (not daily). Requires `GITLAB_MCP_CURSOR_KEY`. Ongoing is bounded `known_mrs` seeds with PRESENT `system=false` note participation.
- `get_merge_request_review_context` — one non-mutating batch tool for 1..10 merge requests. Each proved MR gets its own `review_context` token bound to that owner project and IID. The token carries that MR's source/target ids, branches, refs, version, requested/complete/excluded masks, and semantic digests. Section names are `metadata`, `approvals`, `discussions`, `pipeline_graph`, and `diff_manifest`. `discussions` (RVG-139) reads paged MR discussions. Omit `discussion_selection` for `semantic`; JSON null or any other value is an input error. A fresh exhaustive `all` walk from page 1 can set `content_complete=true` and one `Digests["discussions"]` bundle. Semantic results, partial pages, and resumed tails do not, and their full revision digest stays null. Continuation is a `dc1` cursor, not the context ref. `pipeline_graph` (RVG-140) stays unsupported with `content_complete=unknown` and no HTTP, and cannot be complete evidence. `diff_manifest` (RVG-141) is collected by the shared bounded provider after fork authorization. It is complete only with evidence `diff_manifest.v1`. Its `next_cursor` stays null, so this tool does not resume a `dm1` window. `patch_coverage` stays unknown. Binding checks an independently supplied live ref tuple and fails closed when that observation is missing. A metadata-only complete mask cannot satisfy a later demand for `approvals` or a full review. Missing siblings do not erase a proved sibling ref. The aggregate sets `atomic_snapshot=false` and `signature_attests_review=false`. Signature lifetime is 2h; write freshness is 5 minutes and is not renewed. The signature is authenticity, not an intellectual review. Requires `GITLAB_MCP_CURSOR_KEY`. Caller budget fields, when present, must be positive and within 1000 items / 8 MiB / 30s / 128 requests; zero is rejected. Review profiles only (not daily).
- `update_merge_request`
- `approve_merge_request`
- `unapprove_merge_request`
- `get_merge_request_approval_state` — normalized approval read with evidence-based
  `/approval_state` → `/approvals` fallback (see [`approval-reads.md`](approval-reads.md))
- `get_merge_request_diffs` — first page (`per_page` 100) of MR diffs as an object
  `{diffs, pagination, section}`. Preserves the existing `diffs` array field; adds
  honest pagination (`next_page`) and presence-aware `section` completeness
  (see [`read-envelopes.md`](read-envelopes.md)). Does not walk further pages.
  Local `truncate_lines` marks patch content incomplete. Continuation via
  informational `section.next_cursor` / `pagination.next_page` only (unsigned page
  hint; not an authenticated cursor).
- `list_merge_request_diffs` — lists MR diffs with pagination; additive `section` completeness envelope (see [`read-envelopes.md`](read-envelopes.md))
- `get_merge_request_conflicts` — returns authoritative `has_conflicts` /
  `detailed_merge_status` plus a heuristic `conflict_files` marker scan of the
  inspected first page (`per_page` 200). Scan coverage is described in `section`;
  an incomplete/empty scan never overrides GitLab mergeability flags. Empty scans
  keep legacy `"conflict_files": null` (not `[]`).

  Example (authoritative conflict flags; empty heuristic scan):

  ```json
  {
    "has_conflicts": true,
    "detailed_merge_status": "conflict",
    "conflict_files": null,
    "merge_request_iid": 1,
    "pagination": { "next_page": 0 },
    "section": {
      "retrieved_at": "2026-10-03T15:00:00Z",
      "source": "gitlab_rest",
      "provider": "gitlab",
      "capability_version": "readmeta.mr_diffs.v1",
      "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "pagination_exhausted": true,
      "content_complete": "true",
      "consistency": "consistent",
      "limitations": [
        {
          "code": "partial",
          "message": "conflict_files is a heuristic marker scan of the inspected diffs page only; does not override has_conflicts or detailed_merge_status"
        }
      ],
      "next_cursor": null,
      "counts": { "items": 1, "bytes": 90, "files": null },
      "manifest_coverage": "full",
      "patch_coverage": "full"
    }
  }
  ```
- `list_merge_request_changed_files`
- `get_merge_request_file_diff` — diffs for requested paths from the inspected
  first page (`per_page` 200) as `{diffs, pagination, section}`. A requested path
  missing from a partial page is **unobserved** (limitation), not conclusively
  absent. Empty/`files:[]` keeps legacy `"diffs": null` (not `[]`).

  Example (unobserved requested path on a partial page):

  ```json
  {
    "diffs": null,
    "pagination": { "next_page": 2 },
    "section": {
      "retrieved_at": "2026-10-03T15:00:00Z",
      "source": "gitlab_rest",
      "provider": "gitlab",
      "capability_version": "readmeta.mr_diffs.v1",
      "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "pagination_exhausted": false,
      "content_complete": "false",
      "consistency": "consistent",
      "limitations": [
        {
          "code": "partial",
          "message": "requested file(s) not observed on inspected page (unobserved, not proven absent): missing.go"
        }
      ],
      "next_cursor": "2",
      "counts": { "items": 0, "bytes": 80, "files": null },
      "manifest_coverage": "partial",
      "patch_coverage": "partial"
    }
  }
  ```
- `list_merge_request_versions`
- `get_merge_request_diff_window` — non-mutating window over one proved merge-request diff manifest (default `mode=manifest`), or selected-path bounded diff content when `mode=content`. Full history is `versions/{id}` (or one exact base/start/head tuple). Incremental compare is explicitly partial and only after both SHAs are proved on one authorized project via `commit.id`. Ordinary commit JSON may omit `project_id`; a present `project_id` must match. Manifest mode returns no patch text and no current `/diffs` relabel. Content mode requires `paths` (1..`per_page` distinct repository-relative selectors), optional `context_lines` (0..20, default 3), `max_lines` (1..10000, default 1000), and `max_content_bytes` (1..1048576, default 262144); it returns cropped windows with `window_hash` / `returned_content_hash` scopes and `full_patch_hash=null`. Content mode rejects cursors; manifest continuation remains `dm1`. `per_page` is 1..50. Requires `GITLAB_MCP_CURSOR_KEY`.
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

- `execute_graphql` — selected GraphQL query/mutation (in the daily set).
  Variables are a JSON object (omit/null → `{}`). Optional `operation_name`
  selects among multiple operations via AST parsing (subscriptions rejected).
  Read-only mode allows only the selected query; configured project/group
  allowlists disable the tool (registration + fail-closed handler). Review
  profiles never expose raw GraphQL. Nonempty top-level GraphQL `errors`
  (including HTTP 200 partial data) become tool errors; `errors: []` is success.
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
