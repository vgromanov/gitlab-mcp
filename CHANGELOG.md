# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

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

- Go module and imports are now **`github.com/vgromanov/gitlab-mcp`**, matching
  the canonical GitHub remote; badges, docs, issue template links, GoReleaser
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
