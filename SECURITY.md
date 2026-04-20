# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Email the maintainers privately or use GitHub's
[private security advisory](https://github.com/vgromanov/gitlab-mcp/security/advisories/new) flow. Include:

- A description of the issue and its impact
- Steps to reproduce (PoC welcome)
- Affected version / commit
- Any suggested mitigation

You should receive an acknowledgement within **3 business days**. We aim to
ship a fix or mitigation within **30 days** for high-severity issues.

## Supported versions

This project follows [SemVer](https://semver.org/). Until `1.0.0`, only the
latest minor release receives security fixes.

| Version | Status |
|---|---|
| `0.x` (latest minor) | Supported |
| Older `0.x` minors | Best effort |

## Scope

In scope:

- The `gitlab-mcp` binary, its tool handlers, and HTTP transport.
- The Dockerfile and release artifacts published from this repo.
- Default configurations shipped in this repo.

Out of scope:

- Vulnerabilities in upstream dependencies (please report them upstream;
  we will pick up the fix).
- Misconfiguration on the operator side (e.g. PAT with `api` scope handed to an
  untrusted agent, exposing streamable HTTP without TLS or auth, running with
  `GITLAB_INSECURE=true`).
- Issues only reachable when running with `--insecure` / disabled TLS verify.

## Operational hardening checklist

If you run `gitlab-mcp` in production / shared environments:

- **PAT scope**: grant `read_api` only, unless you actually need write tools.
  Use a dedicated bot user, not a human's PAT.
- **Read-only mode**: set `GITLAB_READ_ONLY_MODE=true` for any agent that
  shouldn't mutate.
- **Project allowlist**: set `GITLAB_ALLOWED_PROJECT_IDS` to limit blast radius.
- **Transport**:
  - Prefer **stdio** with the client launching the binary as a subprocess.
  - For **streamable HTTP**, bind to `127.0.0.1` and front with a reverse
    proxy that adds TLS + authentication. Do not expose the raw `:3002`
    listener publicly.
- **TLS**: never set `GITLAB_INSECURE=true` outside local development. Use
  `GITLAB_CA_CERT_PATH` for self-signed CAs.
- **Secrets**: keep the PAT in environment / secret manager, not in
  configuration files committed to git. The provided `.gitignore` ignores
  `.env`; keep it that way.
- **Container**: published release images use a minimal Alpine runtime and run
  as an unprivileged user. Do not add extra capabilities.

## Credit

We're happy to credit reporters in release notes (or keep things anonymous if
you prefer).
