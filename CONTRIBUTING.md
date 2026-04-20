# Contributing to gitlab-mcp

Thanks for your interest. This project aims to be small, focused, and easy to
audit, so contributions that keep it that way are very welcome.

## Ground rules

- Be respectful — see [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
- Report security issues privately — see [`SECURITY.md`](SECURITY.md).
- Discuss large changes in an issue before opening a PR.
- Prefer the smallest change that solves the problem.
- Don't widen the public surface (new tools, new env vars, new flags) without
  a clear use case.

## Development setup

Requirements:

- Go **1.25+**
- `make`
- A GitLab Personal Access Token (only needed for integration tests / manual runs)

```bash
git clone https://github.com/vgromanov/gitlab-mcp.git
cd gitlab-mcp
cp .env.example .env       # fill in GITLAB_PERSONAL_ACCESS_TOKEN if you'll run live
make build
./bin/gitlab-mcp --help
```

## Make targets

| Target | What it does |
|---|---|
| `make build` | Build `./bin/gitlab-mcp` (`CGO_ENABLED=0`) |
| `make dist` | Cross-compile linux/darwin × amd64/arm64 into `dist/` |
| `make build-all` | Cross-compile to `dist/` for linux/darwin/windows × amd64/arm64 |
| `make test` | `go test ./...` |
| `make race` | `go test -race -count=1 ./...` |
| `make cover` | `go test ./...` with `coverage.out` |
| `make test-integration` | `go test -tags=integration` (needs `.env`) |
| `make lint` | `go vet ./...` + gofmt check |
| `make fmt` | `gofmt -w .` |
| `make vet` | `go vet ./...` |
| `make tidy` | `go mod tidy` |
| `make docker` | Build local container image |
| `make clean` | Remove `bin/`, `dist/`, `coverage.out` |
| `make help` | List targets |

Run `make fmt vet test` (or `make all`) before pushing. CI also runs tests with
the race detector (`make race`).

## Adding a new tool

1. Create or extend a file in `internal/tools/` (group by area, e.g. `issues.go`).
2. Define the request/response types and the handler with the
   `func(ctx, *mcp.CallToolRequest, Args) (*mcp.CallToolResult, Result, error)`
   shape used by other tools.
3. Register the tool inside the area's `Register*` function via `AddTool(...)`.
   - Set the `mutating` argument (`true`/`false`) honestly — it controls
     read-only mode.
   - Use a feature gate (`"pipeline"`, `"milestone"`, `"wiki"`) when the tool
     belongs to a gated surface.
4. Add the `Register*` call to `internal/tools/register.go` if it's a brand new
   group.
5. Add a unit test in `internal/tools/<area>_test.go` using the mock GitLab in
   `internal/testutil/mockgitlab.go`.
6. If the tool needs live API coverage, add an integration test gated by the
   `integration` build tag (see `internal/tools/projects_integration_test.go`).
7. Document it in `docs/tools.md`.

### Naming conventions

- snake_case tool names matching the GitLab REST verb where possible
  (`list_*`, `get_*`, `create_*`, `update_*`, `delete_*`).
- Descriptions: one sentence, no trailing period.
- Required vs optional inputs follow GitLab's API; mark optional fields with
  `[]string{"null", T}` JSON Schema unions, as elsewhere in the codebase.

## Testing

- **Unit**: `make test`. These run against an in-process mock and must stay
  hermetic.
- **Integration**: `make test-integration`. Requires `GITLAB_PERSONAL_ACCESS_TOKEN`
  in `.env`. Tests that mutate are gated by `INTEGRATION_ALLOW_WRITE=1` and
  scoped to `GITLAB_TEST_PROJECT_ID` / `GITLAB_TEST_NAMESPACE`.
- **Schema**: `internal/tools/schema_test.go` validates that every registered
  tool's input schema round-trips and matches expectations. Keep it green.

## Commit & PR conventions

- Conventional-ish commit subjects help reviewers:
  `feat: add list_pipeline_trigger_jobs`, `fix(issues): handle empty labels`,
  `docs: clarify HTTP transport`, `chore: bump client-go to v2.21.0`.
- Squash trivial fixups before requesting review.
- PR description should answer:
  1. What user-visible behavior changes?
  2. Any new env vars / flags / breaking changes?
  3. How was it tested?

## Release

Releases are cut from `main` with a signed tag (`vX.Y.Z`); GoReleaser builds
binaries, `SHA256SUMS`, optional container manifests, and GitHub release assets.
See [`docs/release.md`](docs/release.md) for the full checklist.

## License

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
