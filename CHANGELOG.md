# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security

- Project outputs are allowlisted (default-deny): `get_project`, `list_projects`,
  `list_group_projects`, `search_repositories`, `create_repository` and
  `fork_repository` return only identity, location and state fields (`id`,
  `name`, `path`, `path_with_namespace`, `description`, `default_branch`,
  `visibility`, URLs, `topics`, `archived`, `empty_repo`, timestamps, and reduced
  `namespace` / `forked_from_project`). `runners_token`, `owner`, `permissions`
  and CI/runner/mirror/registry settings are no longer returned (RVG-160).
- The GitLab client no longer retries automatically on anything but GET/HEAD, so
  a write that gets a 502/503 is sent once and cannot be published twice. Read
  retries are unchanged. GraphQL queries (`POST /api/graphql`) are no longer
  retried either (RVG-160).
- `execute_graphql` is safe to expose: `variables` is declared and handled as a
  JSON object (the schema used to say array of numbers); a document containing a
  `mutation` is rejected before any HTTP request when `GITLAB_READ_ONLY_MODE` /
  `--read-only` is set (a small in-repo scanner, no new dependency); top-level
  GraphQL `errors` in an HTTP 200 response now return a tool error; the tool is
  annotated not read-only (RVG-159).

### Fixed

- `get_merge_request_approval_state` falls back to the legacy `/approvals`
  endpoint only on 404; 403/5xx now surface as errors, and the fallback returns
  the same shape (`approval_rules_overwritten`, `rules`) as the primary endpoint
  (RVG-160).

### Added

- `create_merge_request_thread`, `create_merge_request_discussion_note` (reply)
  and `resolve_merge_request_thread` are guarded in the review profile.
  `expected_sha` (the head you reviewed) is required (a missing one is an input
  error before any request; a stale one is refused as `head_changed` with the
  current head, nothing written); thread and reply take an optional `op_key`
  (a retry with the same key returns the existing note, `deduplicated: true`).
  Results carry `written`, `deduplicated`, `note_id`, `discussion_id`,
  `head_sha`, `head_changed_after_write`, `body_modified`, `lines_changed` and
  `error` (set, with IsError, when a readback fails). An inline `position` is
  checked before the write: its `base_sha` / `start_sha` / `head_sha` must equal
  the MR's current `diff_refs` (`anchor_stale`), its path must be a changed file
  (`anchor_not_in_diff`; `anchor_unverifiable` when the diff pages cap is hit)
  and its line must be on the side of the diff it names; GitLab's 400
  `line_code` is `anchor_invalid` and a 5xx on the POST is `gitlab_error`. A bad
  anchor is never turned into a general note. Resolve reads the thread, writes
  nothing when it already is in the requested state, otherwise writes and reads
  the state back. Other profiles keep today's behaviour and output. No tool or
  profile count changes (RVG-171).
- `approve_merge_request`: in the review profile `sha` is required (a missing or
  blank `sha` is an input error before any request) and GitLab refuses a stale
  one (`head_changed`, with the current head in the message). The result is read
  back: `{approved, sha, head_sha, head_changed_after_write, approvals_left,
  approval_state}`; a failed readback sets `error` and is never reported as
  success. Self-approval and other rejections come back as `approval_not_allowed`
  (401/403) or `not_found`. Other profiles keep `sha` optional and the raw
  approvals object. No tool or profile count changes (RVG-172).
- `merge_merge_request` accepts `sha`, `squash` and `auto_merge` (merge when the
  pipeline succeeds) next to `should_remove_source_branch`; each maps to the
  GitLab merge parameter and is sent only when given. One request, never retried
  and never repeated: the result is the merge request as before plus `result`
  (`merged` only when its state is merged, from the response or one re-read;
  otherwise `pending` with `pending_reason` `auto_merge_scheduled` or
  `not_merged_yet`, and `readback_error` if the re-read failed). Refusals are
  errors: `head_changed` (stale `sha`, 409), `not_mergeable` (405/406/422, with
  the MR `state` and `detailed_merge_status`), `merge_not_permitted` (401/403),
  `not_found`. The tool is still in the daily set and, through it, the review
  profile (RVG-172).

- Shared write guard for review-profile note writes (`internal/tools/review_write.go`;
  not wired to any tool yet, RVG-171/172 call it, so tool counts are unchanged).
  `expected_sha` is required and a stale head is refused with `head_changed`
  (current SHA included) before anything is written; an optional `op_key` is
  appended as an invisible `<!-- gitlab-mcp:op=KEY -->` marker and a retry finds
  the existing note (`deduplicated: true`, no second write), counting only notes
  written by the current user and never system notes; if the search hits its page
  cap the write is refused (`dedupe_incomplete`, `complete=false`) rather than
  risk a duplicate; after the write the note and the MR head are read back
  (`head_changed_after_write`, and a failed readback is reported, never taken as
  success). Note bodies are sanitised before they are sent and the result says
  only `body_modified` / `lines_changed`. The RVG-168 spike showed GitLab does not
  reject a stale head on notes, so passing `expected_sha` on as
  `merge_request_diff_head_sha` (ticket step 4) is deliberately not implemented
  (RVG-169).
- `get_server_info`: a read-only, idempotent tool that returns the serving
  build, `{version, revision, revision_short, vcs_time, modified, profile,
  tool_count}`, for clients that cannot read `serverInfo` (the mcp-wrapper bridge
  does not forward the child's). It makes no GitLab request (works offline and
  with an invalid token); build info the binary lacks is reported as `unknown`.
  `profile` is `review`, `daily`, `default` or `custom`, and `tool_count` is the
  number of tools this instance registered (itself included). Registered in the
  review profile (now 55 tools) and in the default catalog; the daily set is
  unchanged at 41 (RVG-177).
- The build revision is visible at runtime: MCP `serverInfo.version` (read after
  `initialize`), `gitlab-mcp -version` and a startup log line now report
  `<version>+<revision>`, where `<revision>` is the first 12 characters of the
  commit the binary was built from (Go's embedded `vcs.revision`), with `-dirty`
  appended when the tree had uncommitted changes, for example
  `0.1.0+bf02f09afdad` or `0.1.0+bf02f09afdad-dirty`. A build without VCS
  information (outside a git checkout, a linked `git worktree`, `-buildvcs=false`,
  a Docker build without `.git`) reports `0.1.0+unknown` and starts normally. No tool, profile or
  dependency change (RVG-176).
- `get_review_queue(group_id, roles?, state?, updated_after?, per_page?,
  max_pages?)`: the current user's review queue in a group. It resolves the
  current user, pages the group MR list per role (`reviewer_id` / `author_id`,
  newest first, `max_pages` default 5 / max 20 pages of `per_page` default 100),
  deduplicates by `(project_id, iid)` and returns one row
  `{project_id, iid, title, web_url, author, reviewers, sha, updated_at, roles}`
  plus `complete`, `truncated_reason` (null when complete) and `current_user`.
  `complete` is false when a role still had a next page at the cap. The row
  `sha` is a list hint; `get_review_snapshot` is the authority for the head SHA.
  `updated_after` (RFC3339 or `YYYY-MM-DD`) is sent at whole-second
  granularity. Registered in the review profile only (now 51 tools; the daily
  set is unchanged) and in the default catalog (RVG-162).
- `get_review_snapshot(mrs, include?, changes_max_pages?)`: reads up to 10 MRs
  `{project_id, iid, expected_sha?}` in one call (sequential upstream requests,
  no cache). Per MR it returns metadata (title, web_url, author, reviewers,
  state, draft, source/target branch, `sha`, `diff_refs`,
  `detailed_merge_status`, `updated_at`; the deprecated `merge_status` is not
  modelled by the SDK), `head_changed` (+ `expected_sha`) when `expected_sha`
  was given, `changes` (changed files with new/renamed/deleted/collapsed/
  too_large flags, no patches; `changes_max_pages` default 3, max 10 pages of
  100; `complete`, `truncated_reason`, `next_page`) and `approvals` (the
  `get_merge_request_approval_state` read, with its 404-only legacy fallback).
  `include` accepts `changes`, `approvals`, `discussions`, `pipeline`; omitted
  means every implemented section, `[]` metadata only, and the not yet
  implemented `pipeline` is rejected. A failing MR (e.g. 404)
  or section is reported as an `error` entry; the rest of the batch still
  returns. Registered in the review profile only (now 52 tools; the daily set is
  unchanged) and in the default catalog (RVG-163).
- `get_review_snapshot` gains the `discussions` section and an end-of-snapshot
  head recheck (RVG-164). `discussions` reads pages of 100 discussions up to
  `discussions_max_pages` (default 3, max 10) and returns `{discussions:
  [{id, resolvable, resolved, notes: [{id, author {id, username, name}, body,
  created_at, updated_at, position?, system}]}], unresolved_count, complete,
  truncated_reason, next_page}`; system notes (and discussions that only hold
  them) are left out unless `include_system` is true; a failed page makes the
  section an `error`, never a half list; `unresolved_count` is a lower bound
  when `complete` is false. After all sections are read the MR head is read
  again: `head_changed` is true when it moved meanwhile (`current_sha` is the new
  head; `sha` stays the head the sections were read for) or when `expected_sha`
  differs from `sha`, and false once the recheck confirmed a stable head, so
  it is now present on every snapshot that read a section (it used to need
  `expected_sha`); a failed recheck is reported as `head_recheck_error`.
  `include` omitted now also reads `discussions`.
- `batch_get_file_contents(project_id, sha, paths, max_bytes_per_file?)`: reads
  up to 20 files at one exact commit in one call (one sequential `GET
  /repository/files` per path, no cache). `sha` must be a full 40-character
  commit SHA; branch and tag names (and short or 64-character ids) are rejected
  before any request. Per path it returns `{path, blob_id, size, binary,
  truncated, content}` or `{path, error}`: a missing or unreadable path (404,
  403, ...) is an error for that file only. `size` is the full blob size;
  `content` is cut at `max_bytes_per_file` (default 32 KiB, max 256 KiB,
  clamped; the effective value is echoed) on a character boundary and flagged
  `truncated`; a binary file (NUL in the first 8000 bytes, or not valid UTF-8)
  is flagged `binary` and its content is not returned. Registered in the review
  profile only (now 53 tools; the daily set is unchanged) and in the default
  catalog (RVG-165).
- `get_pipeline_status(project_id, sha | mr_iid, ref?, jobs?, max_requests?)`:
  the CI status of a commit (or of an MR's head pipeline) in one call, as a
  compact replacement for `get_pipeline` + `list_pipeline_jobs` +
  `list_pipeline_trigger_jobs` (~122 KB and 3+ calls per poll in M2). It reads
  the pipelines, all their jobs and bridges (pages of 100) and follows each
  bridge to its downstream pipeline two levels deep, other projects included
  (sequential requests, no cache; `max_requests` default 60, max 200).
  Returns `overall_status` (`failed` > `canceled` > `running` > `pending` >
  `manual` > `skipped`/`success`; `none` when no pipeline exists yet), `complete`
  + `truncated_reason`, `pipelines[{id, project_id, sha, status, source, depth?,
  parent_pipeline_id?, jobs[{id, name, stage, status, allow_failure?, manual?,
  bridge?}], incomplete?}]` (the flags appear only when true), `failed_jobs`
  (blocking failures), `allowed_failed_jobs`, `manual_jobs` and `requests`. An
  incomplete graph (a child that is inaccessible, not in
  `GITLAB_ALLOWED_PROJECT_IDS` or hidden, the depth limit, the request cap)
  sets `complete=false` and never reports `success` (`unknown`); a visibly
  failed child still fails the whole. `jobs: "problems"` lists only the jobs
  that are not success plus `job_counts` (the compact mode for polling). The
  tool is registered in the review profile (now 54 tools; the daily set is
  unchanged) and in the default catalog (RVG-167).
- `get_review_snapshot`: the `pipeline` section is implemented (the same read as
  `get_pipeline_status` for the MR's head pipeline with `jobs=problems`, at most
  30 upstream requests per MR). It is opt-in: an omitted `include` keeps
  reading `changes`, `approvals` and `discussions` (RVG-167).
- `get_pipeline_job_output` returns a tail window of the trace:
  `tail_lines` (default 200) and `max_bytes` (default 64 KiB, max 1 MiB) select
  the last lines, and the result is `{trace, truncated, total_bytes}`. The trace
  is streamed through a bounded buffer instead of being read whole. `truncate_lines`
  still works but is deprecated: it is now an alias for `tail_lines` and keeps the
  **last** N lines (it used to keep the first N) without the old `... truncated`
  marker line (RVG-158).
- Honest MR diff pagination: `get_merge_request_diffs`,
  `list_merge_request_changed_files` and `get_merge_request_file_diff` accept
  `page`/`per_page` and return `pagination: {page, per_page, next_page,
  complete}` (also on `list_merge_request_diffs`); each call makes one request
  and never pages on. `list_merge_request_changed_files` adds
  `collapsed_files` / `too_large_files`. `page`/`per_page` are now optional in
  every paged tool's input schema (RVG-157).
- `list_merge_requests` accepts `reviewer_id`, `scope`, `updated_after`,
  `updated_before`, `order_by` and `sort` on project, group and global lists;
  invalid `scope` / `order_by` / `sort` / timestamps return a clear input error
  (RVG-156).
- `GITLAB_TOOL_PROFILE=review` (`--tool-profile`): closed 50-tool MR review
  profile (daily set + discussion get/reply/resolve + six pipeline reads, no
  pipeline writes). Daily and default catalogs are unchanged (RVG-155).
- Corp GitLab home `skunk-works/tools/gitlab-mcp` with shared
  `ci-pipelines` `/pipelines/golang.yml`, committed `vendor/`, and
  `.golangci.yml` vendor mode (SW-148).
- Additive tool selection gates: `USE_DAILY_TOOLS`, family flags
  (`USE_ISSUES`, `USE_WORK_ITEMS`, `USE_LABELS`, `USE_DRAFTS`, `USE_WEBHOOKS`,
  `USE_TIMELINE`), and `GITLAB_ENABLED_TOOLS` / `GITLAB_DISABLED_TOOLS`
  (SW-145). Unset flags keep today's legacy catalog.
- Optional `ref` and `search_type` (`basic`|`advanced`|`zoekt`) on `search_code`,
  `search_project_code`, and `search_group_code` for Zoekt/blob Search API parity;
  tool descriptions note `filename:`/`path:`/`extension:` query filters
  (SW-146).
- Docs for tool-gate semantics, env/flag matrix, Zoekt/search inputs, GraphQL
  census caveat, and `mcp.json` profiles (daily-only, daily+issues, disable
  list, full legacy) in README + `docs/configuration.md` / `tools.md` /
  `architecture.md` (SW-147).
- GitHub Actions **release** workflow (GoReleaser v2, QEMU/Buildx, GHCR login).
- `Dockerfile.goreleaser` for release images; expanded `.goreleaser.yaml` with
  ldflags version injection, documentation bundled into archives, `SHA256SUMS`,
  and multi-arch `ghcr.io/vgromanov/gitlab-mcp` manifests (`:version` + `:latest`).
- `.golangci.yml` (lint/format baseline aligned with common Go OSS defaults).
- Issue forms (`bug_report.yml`, `feature_request.yml`) plus `config.yml` with a
  security advisory contact link.
- Makefile targets: `dist`, `cover`, `race`, `vet`, `help`; `build` now forces
  `CGO_ENABLED=0`.
- `-version` / `--version` CLI output (no PAT required).

### Changed

- Go module and imports moved to
  **`gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp`** (corp home);
  GitHub remains a secondary mirror with optional Actions/GHCR (SW-148).
- Go module and imports were previously **`github.com/vgromanov/gitlab-mcp`**,
  matching the GitHub remote; badges, docs, issue template links, GoReleaser
  ldflags/OCI metadata, and **`ghcr.io/vgromanov/gitlab-mcp`** image names were
  updated together.
- CI runs on **ubuntu-latest** and **macos-latest**, uses `go-version-file`,
  enforces `go mod tidy` drift on Linux, runs tests with `-race`, and adds
  concurrency cancellation.
- Default `Dockerfile` runtime switched to **Alpine 3.21** (still non-root).
- `.gitignore` expanded for editor metadata, coverage artifacts, and `.env.*`.
- Lint-driven cleanups: explicit `Close` error handling, embedded `Pagination`
  call sites, and a few revive/staticcheck nits surfaced by `golangci-lint`.

### Fixed

- `list_merge_requests` on a group ignored `author_id`; it now reaches GitLab
  (RVG-156).

## [0.1.0] - 2025-10-12

### Added

- Initial Go implementation of a GitLab MCP server using PAT auth.
- Stdio and streamable HTTP transports (`STREAMABLE_HTTP`).
- GitLab REST + GraphQL tool surface for projects, repository, merge requests,
  issues, labels, releases, and optional wiki/milestone/pipeline tool groups.
- Feature gates:
  `USE_GITLAB_WIKI`, `USE_MILESTONE`, `USE_PIPELINE`, `GITLAB_READ_ONLY_MODE`.
