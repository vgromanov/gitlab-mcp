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
  before `Content-Length`, or that is longer than `Content-Length`, is
  incomplete: the declared size is dropped, and the error selector does not
  treat that prefix as a finished search. Quoted `Authorization` Bearer/Basic
  credentials are redacted through the closing delimiter. `counts.items` is the
  number of output lines and does not add a synthetic line when the text
  already ends in a newline. `max_lines` keeps the Nth line's terminating
  newline, so a one-line `"one\n"` prefix is complete. An un-ranged prefix or
  error GET that is answered with a nonzero 206 is rejected. A nonempty
  configured token is always scanned, including values shorter than eight
  bytes. A bounded range whose span exceeds `max_scan_bytes` still fetches
  lookbehind and lookahead for redaction, then crops the returned window to
  the scan cap. The 30s timeout and default body budget start before project
  authorization so a stuck identity lookup cannot overrun the deadline.
  After authorization, the remaining byte and request caps are raised by
  whatever identity lookups already charged, so a fitting tail is not cut
  short and group ancestry does not spend the trace GET. A 206 whose
  `Content-Range` total is `*` does not complete an error search. A 206 whose
  span is `bytes 0-1/5` is not object EOF for prefix or error selectors:
  the response body ended, but bytes 2–4 were never read. A 206 whose
  `Content-Range` total is `*` is also not object EOF, so a prefix or
  error body that ends in a cut-off `glpat-` is withheld. Quoted
  Authorization credentials honor escaped quotes (`\"`, `\'`) as part of
  the value, not as the closer. Quoted Authorization values that continue
  across a range cut are withheld even when the lookbehind contains spaces.
  Tail and range windows keep redaction context outside the returned
  bytes, a partial
  206 is not a complete trace, redaction is linear in the trace size, and
  `output_bytes` / `redaction_count` follow the UTF-8 text actually returned.
  The trace byte budget includes the redaction margin, a scan that stops
  before EOF withholds a cut-off credential, and Authorization /
  `PRIVATE-TOKEN` values are redacted through their delimiter. A window that
  starts inside a long credential withholds that leading token, URL userinfo
  is redacted through `@` past 256 bytes, a scan-capped 206 larger than its
  declared `Content-Range` is rejected, and cancelling a blocked trace read
  does not deadlock the budget wrapper. A complete short `Bearer` value is
  redacted, and a range end near `math.MaxInt64` does not wrap when the
  redaction margin is added. Password-only URL userinfo (`https://:secret@host`)
  is redacted, and line-cap span remapping stays linear in the retained
  window. Proven EOF finishes a short unterminated `Bearer` value, userinfo
  that ends at `@` without a password is redacted, a tail line cap keeps the
  end of the last line, and an open-ended range still fetches the redaction
  margin (RVG-136).
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
