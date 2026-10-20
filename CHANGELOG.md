# Changelog

All notable changes are tracked here. Versions follow Semantic Versioning.

## 0.1.0 — Unreleased

### Added

- Go backup controller backed by encrypted Restic repositories and S3 storage.
- Explicit repository initialization and source/repository identity registration.
- Restart-safe scheduled backups, persistent status, and kernel operation locks.
- Explicit verified snapshot restores into separate empty directories.
- Opt-in retention that requires a successful last backup.
- Non-root Docker packaging and Compose setup/recovery profiles.
- Local repository integration tests, CI, security scans, dependency updates,
  cross-platform binaries, and tag-triggered GHCR/GitHub releases.
