# KeepSync

**Encrypted S3 backups. Predictable restarts. Explicit restores.**

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Security](https://github.com/TrueFix/keepsync/actions/workflows/security.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/security.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

KeepSync is a lightweight backup service built in Go and packaged for Docker. It uses [Restic](https://restic.net/) to store encrypted, deduplicated snapshots in Amazon S3 or S3-compatible storage.

**On restart, KeepSync continues backing up local files to S3. Restoring files always requires an explicit command.**

```text
Backup    Local files ──→ Encrypted snapshots in S3
Restore   Selected snapshot ──→ Separate recovery directory
```

## Features

- **Scheduled snapshots** — incremental backups at a configurable interval.
- **Source validation** — missing identity markers, mismatched volumes, and empty sources block backups.
- **Read-only access** — the backup container cannot modify your source files.
- **Verified restores** — recover a specific snapshot into a new or empty directory.
- **Explicit retention** — cleanup is opt-in and runs separately from backups.
- **Portable deployment** — Linux amd64/arm64 Docker images and Linux/macOS binaries.

## Quick start 🐳

Requires Docker Compose, an S3 bucket, and an existing directory to back up.

### 1. Configure

```bash
git clone https://github.com/TrueFix/keepsync.git
cd keepsync
cp config.example.json config.json
cp .env.example .env
mkdir -p secrets recovery
```

Set your bucket and prefix in `config.json`:

```json
"repository": "s3:s3.amazonaws.com/your-bucket/keepsync/personal-files"
```

Update `.env` with your absolute source and recovery paths and AWS credentials. Save your repository encryption password in `secrets/restic_password` and keep a separate recovery copy.

The container runs as UID/GID `10001:10001`. Give it read access to source files and the password, and write access to the recovery directory. Registration also needs permission to create `.keepsync-source-id` in the source directory.

### 2. Initialize

```bash
docker compose build
docker compose run --rm keepsync init
docker compose --profile setup run --rm register
```

Run `init` only for a new repository. Registration binds the source directory and repository identities to the persistent state volume.

### 3. Start

```bash
docker compose up -d keepsync
docker compose logs -f keepsync
```

A backup runs immediately on startup, then after each configured interval. Failed attempts retry at the same interval. Keep the source marker and state volume across restarts and upgrades, and use one active backup writer per dataset.

## Backup and restore

```bash
# Create a backup now
docker compose run --rm keepsync backup

# View backup status and available snapshots
docker compose run --rm keepsync status
docker compose run --rm keepsync snapshots

# Verify repository integrity
docker compose run --rm keepsync check
```

To restore, choose a snapshot ID and an empty destination under `/restore`:

```bash
docker compose --profile recovery run --rm restore \
  restore --snapshot <snapshot-id> --target /restore/recovered
```

Backing up `/data` restores files beneath `/restore/recovered/data`. The live source is never a restore destination. Recovery also works when the source data is missing; Compose still requires an existing host directory for its source mount.

Local deletions remain recoverable from earlier snapshots until those snapshots expire. With retention enabled in your configuration, run cleanup explicitly:

```bash
docker compose run --rm keepsync prune
```

Cleanup requires a successful last backup and permanently removes expired recovery points. Monitor `status` for the last successful backup time; container uptime alone does not indicate backup health.

## Configuration

See [`config.example.json`](config.example.json) for the complete configuration.

| Setting | Purpose |
|---|---|
| `dataset` | Stable name used to identify this dataset's snapshots |
| `source` | Absolute path to the directory being backed up |
| `repository` | S3 or S3-compatible Restic repository URL |
| `password_file` | Path to the repository encryption password |
| `state_dir` | Persistent registration and backup status |
| `restore_root` | Parent directory for restore destinations |
| `interval` | Backup interval, such as `30m`, `1h`, or `24h` |
| `excludes` | Restic file exclusion patterns |
| `retention` | Opt-in daily, weekly, and monthly snapshot retention |

Source, state, and restore directories must be separate. Configuration uses strict JSON; unknown fields are rejected.

For a native installation, install Restic and place `--config` before the command:

```bash
keepsync --config /path/to/config.json backup
```

## Development

Requires Go 1.26+ and Restic 0.16+ on Linux or macOS.

```bash
make build
KEEPSYNC_REQUIRE_INTEGRATION=1 make check
```

CI runs race detection, real backup/restore tests, Docker smoke tests, linting, and security scans. Valid version tags matching [`VERSION`](VERSION) trigger GitHub releases and multi-architecture images at `ghcr.io/truefix/keepsync`.

[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md)

## License

[MIT](LICENSE). Restic is distributed under the [BSD 2-Clause License](third_party/restic.LICENSE).
