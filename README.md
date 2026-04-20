# gitlab-mcp

[![CI](https://github.com/vgromanov/gitlab-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/vgromanov/gitlab-mcp/actions/workflows/ci.yml)
[![Release](https://github.com/vgromanov/gitlab-mcp/actions/workflows/release.yml/badge.svg)](https://github.com/vgromanov/gitlab-mcp/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/vgromanov/gitlab-mcp.svg)](https://pkg.go.dev/github.com/vgromanov/gitlab-mcp)
[![Go Report Card](https://goreportcard.com/badge/github.com/vgromanov/gitlab-mcp)](https://goreportcard.com/report/github.com/vgromanov/gitlab-mcp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/vgromanov/gitlab-mcp)](go.mod)

A [Model Context Protocol](https://modelcontextprotocol.io/) server for **GitLab**, written in Go.
Authenticates with a Personal Access Token (PAT) and exposes a curated REST + GraphQL tool surface
to MCP-aware clients (Cursor, Claude Desktop, custom agents, etc.).

Built on:

- the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [GitLab `client-go` v2](https://gitlab.com/gitlab-org/api/client-go)

---

## Features

- **PAT auth** against any GitLab instance (gitlab.com, self-managed, on-prem).
- **Two transports**: stdio (default for desktop clients) and streamable HTTP (`/mcp`).
- **~100 tools** across projects, repository, MRs, MR notes/threads/draft notes,
  issues + notes, labels, milestones, releases, pipelines, jobs, artifacts,
  deployments, environments, wiki, search, webhooks, work items / GraphQL,
  and Markdown rendering.
- **Read-only mode** (`GITLAB_READ_ONLY_MODE=true`) for safe agent workflows;
  every mutating tool is gated and not registered when read-only is on.
- **Feature gates** for heavy or rarely-used surfaces: `USE_PIPELINE`, `USE_MILESTONE`,
  `USE_GITLAB_WIKI`.
- **Project allowlist** (`GITLAB_ALLOWED_PROJECT_IDS`) to restrict which projects an
  agent can act on.
- **Self-managed friendly**: custom CA bundle (`GITLAB_CA_CERT_PATH`),
  proxy support, optional TLS skip for dev.
- **Container image** (multi-stage Go + Alpine runtime) and cross-platform release
  builds via GoReleaser (binaries, `SHA256SUMS`, and multi-arch GHCR images).

See [`docs/tools.md`](docs/tools.md) for the full tool catalog.

---

## Install

### Pre-built binaries

Download from the [Releases](https://github.com/vgromanov/gitlab-mcp/releases) page (Linux / macOS / Windows, amd64 / arm64).

### From source

Requires Go **1.25+**.

```bash
git clone https://github.com/vgromanov/gitlab-mcp.git
cd gitlab-mcp
make build              # binary at ./bin/gitlab-mcp
# or:
go install github.com/vgromanov/gitlab-mcp/cmd/gitlab-mcp@latest
```

### Docker

```bash
docker build -t gitlab-mcp .
docker run --rm -i \
  -e GITLAB_PERSONAL_ACCESS_TOKEN \
  -e GITLAB_API_URL \
  gitlab-mcp
```

The default `Dockerfile` produces a small **Alpine 3.21** runtime image with
`ca-certificates`, running as an unprivileged user (`nonroot`, UID `65532`).
Tagged releases also publish multi-arch images to `ghcr.io/vgromanov/gitlab-mcp`
(see `.goreleaser.yaml`).

---

## Quick start

### stdio (default)

```bash
export GITLAB_PERSONAL_ACCESS_TOKEN=glpat-...
export GITLAB_API_URL=https://gitlab.com/api/v4    # optional; this is the default
gitlab-mcp
```

### Streamable HTTP

```bash
export STREAMABLE_HTTP=true
export HOST=127.0.0.1
export PORT=3002
gitlab-mcp
# -> POST/GET MCP frames at http://127.0.0.1:3002/mcp
```

### `.env` file

A `.env` in the working directory is auto-loaded (via `joho/godotenv`). See
[`.env.example`](.env.example).

---

## Configuration

All options are settable via environment variable **or** CLI flag. CLI flags
win when explicitly passed.

| Variable | Flag | Default | Description |
|---|---|---|---|
| `GITLAB_PERSONAL_ACCESS_TOKEN` | `--token` | — | **Required.** GitLab PAT. |
| `GITLAB_API_URL` | `--api-url` | `https://gitlab.com/api/v4` | API base URL. |
| `GITLAB_READ_ONLY_MODE` | `--read-only` | `false` | Hide all mutating tools. |
| `USE_GITLAB_WIKI` | `--use-wiki` | `false` | Register wiki tools. |
| `USE_MILESTONE` | `--use-milestone` | `false` | Register milestone tools. |
| `USE_PIPELINE` | `--use-pipeline` | `false` | Register pipeline / job / deployment / artifact tools. |
| `STREAMABLE_HTTP` | `--streamable-http` | `false` | Serve HTTP transport instead of stdio. |
| `HOST` | `--host` | `127.0.0.1` | HTTP listen host. |
| `PORT` | `--port` | `3002` | HTTP listen port. |
| `GITLAB_PROJECT_ID` | `--default-project` | — | Default project id or path used when a tool omits it. |
| `GITLAB_ALLOWED_PROJECT_IDS` | — | — | Comma-separated allowlist of project ids/paths. |
| `GITLAB_CA_CERT_PATH` | `--ca-cert` | — | Extra PEM CA bundle for the API client. |
| `GITLAB_INSECURE` | `--insecure` | `false` | Skip TLS verify (**dev only**). |
| `HTTP_PROXY` / `HTTPS_PROXY` | — | — | Standard proxy variables. |

See [`docs/configuration.md`](docs/configuration.md) for examples and trade-offs.

### PAT scopes

The minimum scope is `read_api`. For mutating tools, grant `api`. To use
release-asset upload or wiki edits, ensure the token has the matching
project / group permissions.

## Documentation

- Architecture: [`docs/architecture.md`](docs/architecture.md)
- Configuration: [`docs/configuration.md`](docs/configuration.md)
- Tool catalog: [`docs/tools.md`](docs/tools.md)
- Release checklist: [`docs/release.md`](docs/release.md)
- Contributing: [`CONTRIBUTING.md`](CONTRIBUTING.md)
- Code of conduct: [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md)
- Security: [`SECURITY.md`](SECURITY.md)

---

## Client setup

### Cursor / Claude Desktop (stdio)

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.com/api/v4",
        "GITLAB_READ_ONLY_MODE": "true"
      }
    }
  }
}
```

### Streamable HTTP client

Point your MCP client at `http://HOST:PORT/mcp`. Run the server with
`STREAMABLE_HTTP=true` and bind to `127.0.0.1` unless you also put a
TLS-terminating reverse proxy in front.

---

## Tool surface (overview)

| Group | Examples |
|---|---|
| Projects / namespaces / users | `list_projects`, `get_project`, `list_namespaces`, `get_users` |
| Repository | `get_file_contents`, `get_repository_tree`, `list_commits`, `get_branch_diffs`, `create_branch`, `push_files` |
| Merge requests | `list_merge_requests`, `get_merge_request`, `get_merge_request_diffs`, `create_merge_request`, `merge_merge_request`, `*_merge_request_note(s)`, draft notes, threads |
| Issues | `list_issues`, `my_issues`, `create_issue`, `update_issue`, `*_issue_link`, `*_issue_note` |
| Labels / milestones | `list_labels`, `create_label`, `list_milestones`, `*_milestone` (gated) |
| CI/CD | `list_pipelines`, `get_pipeline_job_output`, `play_pipeline_job`, `list_deployments`, `list_environments`, `*_job_artifacts` (gated by `USE_PIPELINE`) |
| Releases | `list_releases`, `create_release`, `download_release_asset` |
| Wiki | `list_wiki_pages`, `create_wiki_page` (gated by `USE_GITLAB_WIKI`) |
| Search | `search_code`, `search_project_code`, `search_group_code`, `search_repositories` |
| Markdown | `upload_markdown`, `download_attachment` |
| Webhooks | `list_webhooks`, `list_webhook_events`, `get_webhook_event` |
| GraphQL / work items | `execute_graphql`, `list_work_items`, `get_work_item`, `create_work_item` |

The full machine-readable list is in [`docs/tools.md`](docs/tools.md). Mutating
tools are suppressed when `GITLAB_READ_ONLY_MODE=true`.

---

## Development

```bash
make test                 # unit tests
make race                 # race detector (same packages as CI)
make lint                 # go vet + gofmt check
make test-integration     # needs .env with PAT (build tag: integration)
make build                # ./bin/gitlab-mcp
make dist                 # linux/darwin × amd64/arm64 into dist/
make build-all            # cross-compile including windows/amd64
make docker               # build local image
```

Architecture notes: [`docs/architecture.md`](docs/architecture.md).
Contribution guide: [`CONTRIBUTING.md`](CONTRIBUTING.md).

---

## Security

If you find a vulnerability, please follow [`SECURITY.md`](SECURITY.md) — do not
file a public issue.

Operational guidance:

- Treat the PAT as a secret; never commit `.env`.
- Prefer `GITLAB_READ_ONLY_MODE=true` for any agent that doesn't need to write.
- Use `GITLAB_ALLOWED_PROJECT_IDS` to scope agent reach.
- Bind streamable HTTP to loopback unless you front it with TLS + auth.

---

## License

[MIT](LICENSE).

## Acknowledgements

- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)
- [gitlab-org/api/client-go](https://gitlab.com/gitlab-org/api/client-go)
- Prior art and tool naming conventions from the broader MCP ecosystem.
