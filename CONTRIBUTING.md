# Contributing

Open an issue for larger changes, then submit a focused pull request.

1. Install Go 1.27.3+ on Linux or macOS.
2. Run `make check`. Recovery tests run without external backup tools.
3. Run `make image-test` for Docker or packaging changes. This builds the image
   and exercises environment-based setup, backup, integrity checking, and recovery.
4. Releases publish Linux amd64/arm64 Docker images; local Go builds are for development.
5. Document user-visible changes in `CHANGELOG.md`.

Safety invariants must remain intact: startup initializes only an unclaimed
target and never restores. Setup must work with a read-only source. Existing
state and target identities are verified, and restore never targets live source;
snapshot retention runs only after a confirmed snapshot; failed operations never
become successful status entries. Watch-triggered work must stay scoped, bounded,
and serialized with snapshot creation.

Run `make build && go run ./scripts/resourcecheck` on Linux to measure actual
idle RSS/CPU against an S3 emulator. The default fixture has 1,000 small files.
CI enforces a 40 MiB unpacked runtime-image budget.

Add meaningful regression tests when changing these boundaries. Do not commit
credentials, source data, local configuration, or artifacts.

To release, update `VERSION`, published image references, `.env.example`,
and `CHANGELOG.md`, then push a matching annotated `vMAJOR.MINOR.PATCH` tag after
CI passes. The release workflow tests that exact commit and publishes the Docker
image and release notes. Never retag a release.

Test both `compose.yaml` and `compose.advanced.yaml` when changing deployment
examples. Both use the same TAR snapshot engine.

Docker Hub releases publish `mayankfawkes/keepsync:<version>` and update `latest`
for stable versions only. Create that Docker Hub repository, then add these
GitHub repository Actions secrets before tagging:

- `DOCKERHUB_USERNAME`: Docker Hub login with access to the repository.
- `DOCKERHUB_TOKEN`: Docker Hub access token with read/write permission.

The workflow uses GitHub's token for GitHub release notes and provenance only.
If a release fails, fix the cause and use the Release workflow's manual run with
the existing tag; do not move the tag.
