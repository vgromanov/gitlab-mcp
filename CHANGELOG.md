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
  implemented `discussions` / `pipeline` are rejected. A failing MR (e.g. 404)
  or section is reported as an `error` entry; the rest of the batch still
  returns. Registered in the review profile only (now 52 tools; the daily set is
  unchanged) and in the default catalog (RVG-163).
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
