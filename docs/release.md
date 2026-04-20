# Release Checklist

This project publishes versioned binaries and checksums with GoReleaser.

## Preconditions

- `main` is green (`make fmt vet test build`).
- `CHANGELOG.md` has an entry for the target version.
- `README.md` and docs reflect any user-visible changes.

## Versioning

- Follow Semantic Versioning.
- Tag format: `vX.Y.Z` (for example `v0.2.0`).

## Local verification

```bash
make clean
make fmt vet test build
make dist
```

Optional dry run:

```bash
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

## Cut release

```bash
git checkout main
git pull --ff-only
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

CI/release automation runs GoReleaser from `.github/workflows/release.yml`
using `.goreleaser.yaml`.

## Post-release

- Verify release assets exist for:
  - linux amd64/arm64
  - darwin amd64/arm64
  - windows amd64
- Verify `SHA256SUMS` is attached.
- Smoke test at least one binary and the `ghcr.io/vgromanov/gitlab-mcp` image tags
  published for the release.
- Announce highlights from the changelog.
