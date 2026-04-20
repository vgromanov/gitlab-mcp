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
| `HTTP_PROXY` / `HTTPS_PROXY` | — | empty | Standard outbound proxy env vars. |

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

## PAT scope guidance

- For read-only workflows, prefer `read_api`.
- For write operations, use `api` on a dedicated bot account.
- Scope project/group membership as tightly as practical.
