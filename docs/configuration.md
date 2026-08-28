# Configuration Guide

`gitlab-mcp` reads config from environment variables and optional CLI flags.
When a CLI flag is explicitly provided, it overrides the environment value.

## Required

- `GITLAB_PERSONAL_ACCESS_TOKEN` (`--token`): GitLab PAT used for API calls.

## Core settings

| Variable | Flag | Default | Notes |
|---|---|---|---|
| `GITLAB_API_URL` | `--api-url` | `https://gitlab.com/api/v4` | Set for self-managed GitLab. |
| `GITLAB_READ_ONLY_MODE` | `--read-only` | `false` | Hides mutating tools completely. |
| `GITLAB_PROJECT_ID` | `--default-project` | empty | Default project if a tool omits `project_id`. |
| `GITLAB_ALLOWED_PROJECT_IDS` | — | empty | Comma-separated allowlist. |

## Tool selection gates

Catalog membership is decided in `internal/tools/selection.go` after the
read-only filter. **Restricted mode** turns on when any of these is set:

- `USE_DAILY_TOOLS=true`
- any **new** family flag (`USE_ISSUES`, `USE_WORK_ITEMS`, `USE_LABELS`,
  `USE_DRAFTS`, `USE_WEBHOOKS`, `USE_TIMELINE`)
- non-empty `GITLAB_ENABLED_TOOLS`

Legacy `USE_PIPELINE` / `USE_MILESTONE` / `USE_GITLAB_WIKI` alone do **not**
enter restricted mode (core catalog + that family).

| Mode | Base set | Then |
|---|---|---|
| Unrestricted (no flags above) | Today's legacy catalog: all ungated tools; `pipeline` / `milestone` / `wiki` only if their `USE_*=true` | Subtract `GITLAB_DISABLED_TOOLS` |
| Restricted | Empty, then **union** of every true enable source | Subtract `GITLAB_DISABLED_TOOLS` |

**Enable sources (additive union in restricted mode):**

| Source | Effect |
|---|---|
| `USE_DAILY_TOOLS` | Pinned Aug-2026 census set (**41** tools, `band` core\|rare). Always includes `search_code`, `search_project_code`, `search_group_code`, `search_repositories`. |
| Family flags | Whole family tool sets (new flags + legacy `USE_PIPELINE` / `USE_MILESTONE` / `USE_GITLAB_WIKI` when true) |
| `GITLAB_ENABLED_TOOLS` | Comma-separated tool names |

`GITLAB_DISABLED_TOOLS` always subtracts (both modes). Unknown names in
enable/disable lists log a startup warning and are ignored.

**Backward compat:** unset everything → same catalog as before SW-145
(pipeline / wiki / milestone still default **off**). Setting only
`USE_PIPELINE=true` stays legacy (core + pipeline), not restricted-only-pipeline.

Recommended Cursor profile for ship/MR work: `USE_DAILY_TOOLS=true` alone
(MR tools + the four search tools).

### Env / flag matrix

| Variable | Flag | Default | Restricted? | Notes |
|---|---|---|---|---|
| `USE_DAILY_TOOLS` | `--use-daily-tools` | `false` | Yes | 41-tool daily set. |
| `USE_ISSUES` | `--use-issues` | `false` | Yes | Issues + issue notes/links. |
| `USE_WORK_ITEMS` | `--use-work-items` | `false` | Yes | Work-item GraphQL tools (not `execute_graphql`). |
| `USE_LABELS` | `--use-labels` | `false` | Yes | Label CRUD. |
| `USE_DRAFTS` | `--use-drafts` | `false` | Yes | MR draft notes. |
| `USE_WEBHOOKS` | `--use-webhooks` | `false` | Yes | Webhook list/events. |
| `USE_TIMELINE` | `--use-timeline` | `false` | Yes | Timeline events. |
| `USE_PIPELINE` | `--use-pipeline` | `false` | No\* | Pipeline / job / deployment / artifact tools. |
| `USE_MILESTONE` | `--use-milestone` | `false` | No\* | Milestone tools. |
| `USE_GITLAB_WIKI` | `--use-wiki` | `false` | No\* | Project/group wiki tools. |
| `GITLAB_ENABLED_TOOLS` | `--enabled-tools` | empty | Yes (if non-empty) | Extra tool names (CSV). |
| `GITLAB_DISABLED_TOOLS` | `--disabled-tools` | empty | — | Always subtracts (CSV). |

\* Alone these stay unrestricted. In restricted mode they still contribute their
family to the enable union when `true`.

### GraphQL census caveat

A REST tool that never appears in agent call logs is not necessarily unused:
agents often reach the same GitLab surface through `execute_graphql` (about
one-third of go-gitlab MCP calls in the Aug-2026 census). Treat “unused REST”
bands cautiously when deciding what to keep in the daily set.

### Example `mcp.json` profiles

#### Daily-only (recommended for ship/MR)

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.example.com/api/v4",
        "USE_DAILY_TOOLS": "true"
      }
    }
  }
}
```

#### Daily + issues

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.example.com/api/v4",
        "USE_DAILY_TOOLS": "true",
        "USE_ISSUES": "true"
      }
    }
  }
}
```

#### Disable list (works in either mode)

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.example.com/api/v4",
        "USE_DAILY_TOOLS": "true",
        "GITLAB_DISABLED_TOOLS": "execute_graphql,create_release"
      }
    }
  }
}
```

#### Full legacy (unrestricted catalog)

Omit `USE_DAILY_TOOLS`, new family flags, and `GITLAB_ENABLED_TOOLS`. Opt into
legacy heavy surfaces as needed:

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.example.com/api/v4",
        "USE_PIPELINE": "true",
        "USE_MILESTONE": "true",
        "USE_GITLAB_WIKI": "true"
      }
    }
  }
}
```

## Code search (Zoekt / blob FTS)

`search_code`, `search_project_code`, and `search_group_code` call the GitLab
Search API with `scope=blobs` (exact code search / Zoekt when enabled on the
instance). Optional MCP inputs:

| Input | Values | Notes |
|---|---|---|
| `search_type` | `basic` \| `advanced` \| `zoekt` | Passed as Search API `search_type` query param. |
| `ref` | branch or tag name | Pins the search to that ref. |

Filter syntax belongs **inside** `query` (not separate MCP fields):

- `filename:*.go` — match file name patterns
- `path:internal/` — restrict by path prefix
- `extension:go` — restrict by extension

Example query: `ShouldRegister filename:selection.go path:internal/tools/`.

See also [`docs/tools.md`](tools.md#search--events--markdown--webhooks).

## HTTP transport

| Variable | Flag | Default | Notes |
|---|---|---|---|
| `STREAMABLE_HTTP` | `--streamable-http` | `false` | Serve MCP over HTTP instead of stdio. |
| `HOST` | `--host` | `127.0.0.1` | Bind host for HTTP server. |
| `PORT` | `--port` | `3002` | Bind port for HTTP server. |

## TLS / network

| Variable | Flag | Default | Notes |
|---|---|---|---|
| `GITLAB_CA_CERT_PATH` | `--ca-cert` | empty | Additional PEM CA bundle. |
| `GITLAB_INSECURE` | `--insecure` | `false` | Skip TLS verify (dev only). |
| `HTTP_PROXY` / `HTTPS_PROXY` | — | inherited | Outbound proxy for the GitLab API client. |
| `NO_PROXY` / `no_proxy` | — | inherited | Hosts that bypass the proxy. Must include your GitLab API hostname. |

The GitLab API client uses Go's standard `http.ProxyFromEnvironment`. It does
**not** read MCP transport settings (`stdio` vs `streamable-http`) — only these
env vars (and optional `HTTP_PROXY` / `HTTPS_PROXY` in config) affect outbound
HTTPS to GitLab.

### Corporate proxy / `503 Service Unavailable`

If the MCP child inherits a corporate `HTTP(S)_PROXY` but `NO_PROXY` does not
cover your GitLab host, API calls are tunneled through the proxy and often fail
with `503 Service Unavailable` (CONNECT tunnel rejected). This is an environment
issue, not an MCP server bug.

**Fix:** in the MCP client's `env` block (Cursor `mcp.json`, Claude Desktop,
etc.):

1. Set `NO_PROXY` / `no_proxy` to include the GitLab API host **and** its
   registrable suffix (suffix form `.example.com` is more portable than
   `*.example.com` for tools like `curl`).
2. Clear proxy vars the child should not inherit: set `HTTP_PROXY`, `HTTPS_PROXY`,
   `ALL_PROXY`, and lowercase variants to `""`.

Derive hosts from `GITLAB_API_URL`. For `https://gitlab.example.com/api/v4` use
at least `gitlab.example.com,.example.com,127.0.0.1,localhost`.

After editing MCP config, **restart the MCP server** in the client — env changes
do not always apply to an already-running child process.

**Sanity check** (expect `401` without a token, not `503`):

```bash
NO_PROXY=".example.com,gitlab.example.com" curl -sS -o /dev/null -w "%{http_code}\n" \
  https://gitlab.example.com/api/v4/version
```

## Recommended profiles

### Safe read-only agent

```bash
export GITLAB_PERSONAL_ACCESS_TOKEN=glpat-...
export GITLAB_READ_ONLY_MODE=true
export GITLAB_ALLOWED_PROJECT_IDS=group/proj-a,group/proj-b
gitlab-mcp
```

### Local HTTP for one host

```bash
export GITLAB_PERSONAL_ACCESS_TOKEN=glpat-...
export STREAMABLE_HTTP=true
export HOST=127.0.0.1
export PORT=3002
gitlab-mcp
```

### Self-managed GitLab with custom CA

```bash
export GITLAB_PERSONAL_ACCESS_TOKEN=glpat-...
export GITLAB_API_URL=https://gitlab.example.com/api/v4
export GITLAB_CA_CERT_PATH=/etc/ssl/certs/gitlab-ca.pem
gitlab-mcp
```

### Cursor / Claude Desktop behind a corporate proxy

```json
{
  "mcpServers": {
    "gitlab": {
      "command": "gitlab-mcp",
      "env": {
        "GITLAB_PERSONAL_ACCESS_TOKEN": "${env:GITLAB_PERSONAL_ACCESS_TOKEN}",
        "GITLAB_API_URL": "https://gitlab.example.com/api/v4",
        "GITLAB_READ_ONLY_MODE": "true",
        "NO_PROXY": ".example.com,gitlab.example.com,127.0.0.1,localhost",
        "no_proxy": ".example.com,gitlab.example.com,127.0.0.1,localhost",
        "ALL_PROXY": "",
        "all_proxy": "",
        "HTTP_PROXY": "",
        "HTTPS_PROXY": "",
        "http_proxy": "",
        "https_proxy": ""
      }
    }
  }
}
```

Replace `example.com` / `gitlab.example.com` with your GitLab host. Keep PAT in
the environment or `${env:...}` — do not commit tokens to the repo.

## PAT scope guidance

- For read-only workflows, prefer `read_api`.
- For write operations, use `api` on a dedicated bot account.
- Scope project/group membership as tightly as practical.
