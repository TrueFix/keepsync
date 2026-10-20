# Contributing

Open an issue for larger changes, then submit a focused pull request.

1. Install Go 1.26+ and Restic 0.16+ on Linux or macOS.
2. Run `KEEPSYNC_REQUIRE_INTEGRATION=1 make check`.
3. Run `make build release` for packaging changes.
4. For Docker changes, build the image and run
   `bash scripts/docker-smoke.sh keepsync:test` with a `keepsync:test` image.
5. Document user-visible changes in `CHANGELOG.md`.

Safety invariants must remain intact: startup never restores or initializes;
missing source identity blocks backups; restore never targets live source;
retention is explicit; failed backups never become successful status entries.

Add meaningful regression tests when changing these boundaries. Do not commit
credentials, source data, repository passwords, local configuration, or artifacts.

Maintainers release using the procedure in the README. Never retag a release.
