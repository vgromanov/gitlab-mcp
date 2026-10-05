# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- `get_pipeline_job_output` returns a bounded, always-redacted trace window
  (`prefix` / `tail` / `error` / `range`) with source offsets, `total_known`,
  `output_bytes`, and `redaction_count`. Legacy `truncate_lines` still selects
  a prefix. Ignored `Range` responses are not scanned without a bound, and a
  scan that cannot prove the tail does not invent one. A 200 body that ends
  before `Content-Length` is incomplete: the declared size is dropped, and the
  error selector does not treat that prefix as a finished search (RVG-136).
- Legacy MR diff getters (`get_merge_request_diffs`, `get_merge_request_file_diff`,
  `get_merge_request_conflicts`) now include honest `pagination` / `section`
  completeness metadata on their existing object responses (RVG-127 /
  LOCAL-GLM-013). Existing field names (`diffs`, `has_conflicts`,
  `detailed_merge_status`, `conflict_files`, …) are preserved. Partial pages,
  local `truncate_lines`, collapsed/too_large/omitted presence, and heuristic
  conflict scans no longer imply false completeness; a missing requested file on
  a partial page is reported as unobserved. See `docs/read-envelopes.md` and
  `docs/tools.md`.

### Added

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

## [0.1.0] - 2025-10-12

### Added

- Initial Go implementation of a GitLab MCP server using PAT auth.
- Stdio and streamable HTTP transports (`STREAMABLE_HTTP`).
- GitLab REST + GraphQL tool surface for projects, repository, merge requests,
  issues, labels, releases, and optional wiki/milestone/pipeline tool groups.
- Feature gates:
  `USE_GITLAB_WIKI`, `USE_MILESTONE`, `USE_PIPELINE`, `GITLAB_READ_ONLY_MODE`.
