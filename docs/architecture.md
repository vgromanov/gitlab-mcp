# Architecture

`gitlab-mcp` is a Go MCP server that maps MCP tool calls to GitLab REST/GraphQL
API operations with explicit safety gates for read-only and feature-scoped
surfaces.

## High-level flow

1. `cmd/gitlab-mcp/main.go`
   - loads runtime config (`internal/config`)
   - initializes GitLab client (`internal/gitlab`)
   - creates MCP server (`internal/mcpsrv`)
   - serves stdio or streamable HTTP (`/mcp`)
2. `internal/mcpsrv/server.go`
   - wires dependencies and registers all tool groups in `internal/tools`
3. `internal/tools/*`
   - each file owns one API domain (projects, repository, MRs, issues, etc.)
   - each handler validates input args, calls the GitLab client abstraction,
     and maps responses to MCP results
4. `internal/tools/allow.go` + `AddTool(...)`
   - mutating tools are blocked when `GITLAB_READ_ONLY_MODE=true`
   - feature-gated tool groups (`pipeline`, `milestone`, `wiki`) register only
     when enabled

## Package layout

- `cmd/gitlab-mcp`: entrypoint and process lifecycle.
- `internal/config`: env + flag config parsing and defaults.
- `internal/gitlab`: API client construction and helpers.
- `internal/mcpsrv`: MCP transport/server setup.
- `internal/tools`: tool registration and handler implementations.
- `internal/testutil`: mock GitLab and integration helpers.

## Safety model

- **Read-only mode**: mutating tools are not registered at all.
- **Project allowlist**: `GITLAB_ALLOWED_PROJECT_IDS` can constrain tool reach.
- **Transport isolation**: stdio is default; HTTP is opt-in and intended to run
  on loopback unless fronted by authenticated TLS.
- **Auth principle**: the PAT defines effective permissions; use least privilege.

## Testing strategy

- Unit tests (`go test ./...`) use mocks in `internal/testutil`.
- Integration tests (`-tags=integration`) exercise live GitLab API behavior and
  are opt-in via env.
- Schema tests verify JSON schema consistency for registered tools.

## Release architecture

- Binary releases are built cross-platform via GoReleaser (`.goreleaser.yaml`).
- Container image is multi-stage built and runs distroless as non-root.
