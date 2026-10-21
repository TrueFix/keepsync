# Changelog

All notable changes are tracked here. Versions follow Semantic Versioning.

## 0.3.2 — 2026-10-21

### Changed

- Store new TAR snapshots directly in lowercase `snapshots/`, with a matching `<snapshot-base>.json` file for source information and integrity metadata.
- Publish metadata after the archive is fully uploaded; listing and retention count complete pairs only.
- Expire the archive and its metadata together while preserving source-specific ownership checks.

### Compatibility

- Existing nested TAR archives remain readable and participate in normal retention without automatic relocation.
- Each TAR still embeds its own recovery metadata; downloading the JSON sidecar is optional for manual extraction.

## 0.3.1 — 2026-10-21

### Fixed

- Initialize source identity and a fresh backup target automatically on startup, without requiring `register-source`, `init`, or a writable `/data` mount.
- Keep source identity in the state volume and verify the mounted directory across restarts; existing marker-based registrations remain supported.
- Automatically initialize encrypted history when first needed, independently of Live uploads.
- Wait quietly for files in an empty source and resume as soon as live changes arrive.
- Preserve recovery identity inside TAR archives and encrypted snapshot metadata without modifying the live source.

## 0.3.0 — 2026-10-21

### Added

- Optional `tar` snapshots: portable, unencrypted `.tar.gz` archives with source metadata and count-based retention.
- TAR integrity checking and isolated recovery with checksum, identity, and path validation.
- A minimal Live + TAR/30-minute Compose example and a separate encrypted advanced configuration.
- Password setup and recovery instructions, including recovery without local state.

### Compatibility

- `restic` remains the default snapshot format; existing encrypted repositories and registrations remain usable.
- TAR history is stored in a separate `Snapshots/tar/<dataset>/<source-id>/` prefix, with independent retention.

## 0.2.0 — 2026-10-20

### Changed

- Add `Live/` file uploads and independently scheduled, optional `Snapshots/` history with count-based retention.
- Add recursive live watching, bounded debounce/max-wait batching, and confirmed per-file upload logs.
- Use an on-disk index, scoped event batches, one upload worker, and a sleeping scheduler to reduce ongoing CPU/memory usage.
- Build a minimal non-root `scratch` image with an S3/local-only snapshot engine and a 40 MiB CI image budget.
- Measure idle resources against an S3 emulator and provide configurable Compose CPU/memory limits.
- Reject legacy registrations instead of implicitly migrating existing backup layouts.
- Make Docker Compose the primary deployment, with environment-based configuration and image-only releases.
- Select JSON configuration explicitly with `--config`; environment configuration is now the default.
- Update Go to 1.27.3 and use Alpine 3.24.2 only for build-time runtime-file preparation.
- Build Restic 0.19.1 consistently in Docker and CI, with separately locked and scanned dependencies.
- Upgrade GitHub Actions to current releases, pinned by commit for reproducible runs.
- Publish versioned multi-architecture images to Docker Hub at `mayankfawkes/keepsync` with SBOMs and provenance.

### Compatibility

- The new `Live/` and `Snapshots/` layout requires a fresh target prefix and state volume; existing root-layout repositories remain untouched.
- Snapshot retention is configured with `snapshots.keep_last` or `KEEPSYNC_SNAPSHOT_RETAIN` instead of the earlier daily/weekly/monthly policy.

## 0.1.0 — 2026-10-20

### Added

- Go backup controller backed by encrypted Restic repositories and S3 storage.
- Explicit repository initialization and source/repository identity registration.
- Restart-safe scheduled backups, persistent status, and kernel operation locks.
- Explicit verified snapshot restores into separate empty directories.
- Opt-in retention that requires a successful last backup.
- Non-root Docker packaging and Compose setup/recovery profiles.
- Local repository integration tests, CI, security scans, dependency updates,
  cross-platform binaries, and tag-triggered GHCR/GitHub releases.
