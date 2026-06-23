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

## Feature gates

| Variable | Flag | Default | Effect |
|---|---|---|---|
| `USE_PIPELINE` | `--use-pipeline` | `false` | Enables pipeline, job, deployment, environment, artifact tools. |
| `USE_MILESTONE` | `--use-milestone` | `false` | Enables milestone tools. |
| `USE_GITLAB_WIKI` | `--use-wiki` | `false` | Enables project/group wiki tools. |

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
