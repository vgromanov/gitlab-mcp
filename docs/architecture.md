# Architecture

`gitlab-mcp` is a Go MCP server that maps MCP tool calls to GitLab REST/GraphQL
API operations with explicit safety gates for read-only mode and additive tool
selection (restricted vs legacy catalog).

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
4. `internal/tools/registry.go` `AddTool(...)` + `selection.go`
   - mutating tools are blocked when `GITLAB_READ_ONLY_MODE=true`
   - `ShouldRegister` applies restricted vs unrestricted catalog rules, then
     subtracts `GITLAB_DISABLED_TOOLS`
   - family tags (`issues`, `pipeline`, `wiki`, …) feed the enable union

```text
Register tool name
  → GITLAB_READ_ONLY and mutating? → skip
  → Restricted mode?
       no  → legacy catalog (pipeline/milestone/wiki only if USE_*=true)
       yes → empty base ∪ daily ∪ families ∪ GITLAB_ENABLED_TOOLS
  → subtract GITLAB_DISABLED_TOOLS
  → mcp.AddTool
```

Restricted mode turns on for `USE_DAILY_TOOLS`, any new family flag
(`USE_ISSUES`, `USE_WORK_ITEMS`, `USE_LABELS`, `USE_DRAFTS`, `USE_WEBHOOKS`,
`USE_TIMELINE`), or non-empty `GITLAB_ENABLED_TOOLS`. Legacy
`USE_PIPELINE` / `USE_MILESTONE` / `USE_GITLAB_WIKI` alone stay unrestricted.
Details and `mcp.json` examples: [`docs/configuration.md`](configuration.md#tool-selection-gates).

## Package layout

- `cmd/gitlab-mcp`: entrypoint and process lifecycle.
- `internal/config`: env + flag config parsing and defaults (including selection
  flags).
- `internal/gitlab`: API client construction and helpers (e.g. Search API
  `search_type` query escape hatch).
- `internal/mcpsrv`: MCP transport/server setup.
- `internal/tools`: tool registration, handlers, and `selection.go` catalog
  logic (`DailyTools`, `FamilyTools`, `ShouldRegister`).
- `internal/testutil`: mock GitLab and integration helpers.

## Safety model

- **Read-only mode**: mutating tools are not registered at all.
- **Additive selection**: default remains the legacy full catalog; restricted
  profiles opt into a smaller union (daily set ± families ± enable list).
- **Project allowlist**: `GITLAB_ALLOWED_PROJECT_IDS` can constrain tool reach.
- **Transport isolation**: stdio is default; HTTP is opt-in and intended to run
  on loopback unless fronted by authenticated TLS.
- **Auth principle**: the PAT defines effective permissions; use least privilege.

## Search / Zoekt path

`search_code` / `search_project_code` / `search_group_code` call Search API
`scope=blobs` (Zoekt FTS when exact code search is enabled). Optional MCP
inputs `search_type` and `ref` are forwarded as query parameters; filter tokens
(`filename:`, `path:`, `extension:`) stay inside `query`.

## GraphQL caveat

REST registration is not the only path to a GitLab surface: `execute_graphql`
often covers work items and APIs without a dedicated REST tool. Census “unused
REST” bands can therefore be misleading — see
[`docs/tools.md`](tools.md#graphql-census-caveat).

## Testing strategy

- Unit tests (`go test ./...`) use mocks in `internal/testutil`.
- Integration tests (`-tags=integration`) exercise live GitLab API behavior and
  are opt-in via env.
- Schema tests verify JSON schema consistency for registered tools.
- Selection tests pin daily-set size (41), required search-tool membership, and
  the restricted/legacy behavior matrix.

## Release architecture

- Binary releases are built cross-platform via GoReleaser (`.goreleaser.yaml`).
- Container image is multi-stage built and runs distroless as non-root.
