# KeepSync

**Live S3 backups with snapshot history, built for Docker.**

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Mount your files at `/data`. KeepSync uploads changes to `Live/` and saves scheduled TAR recovery points directly under `snapshots/`. **Restarts resume backups; restores are always explicit.**

| Snapshot format | Password | Recovery |
|---|---|---|
| `tar` | None; unencrypted `.tar.gz` | Download and extract on your PC |
| `restic` | Required; encrypted and deduplicated | Restore with KeepSync or Restic |

`Live/` contains ordinary files. Your storage provider's server-side encryption is separate from snapshot encryption.

## Live + TAR snapshots every 30 minutes

Use the [Docker Hub image](https://hub.docker.com/r/mayankfawkes/keepsync) for Linux `amd64` or `arm64`.

```yaml
services:
  keepsync:
    image: mayankfawkes/keepsync:0.3.6
    restart: unless-stopped
    read_only: true
    cpus: 0.5
    mem_limit: 256m
    environment:
      KEEPSYNC_TARGET: s3://your-bucket/backups/app
      KEEPSYNC_METHOD: live
      KEEPSYNC_SNAPSHOTS_ENABLED: "true"
      KEEPSYNC_SNAPSHOT_FORMAT: tar
      KEEPSYNC_SNAPSHOT_INTERVAL: 30m
      KEEPSYNC_SNAPSHOT_RETAIN: "7"
      AWS_ACCESS_KEY_ID: ${AWS_ACCESS_KEY_ID}
      AWS_SECRET_ACCESS_KEY: ${AWS_SECRET_ACCESS_KEY}
      AWS_DEFAULT_REGION: us-east-1
    volumes:
      - type: bind
        source: ./data
        target: /data
        read_only: true
        bind:
          create_host_path: false
      - keepsync-state:/var/lib/keepsync

volumes:
  keepsync-state:
```

Set your target and credentials, mount a directory the container can read, then start it:

```bash
docker compose up -d keepsync
```

On the first run, KeepSync initializes its state and binds an unused target automatically. `/data` stays read-only: no source marker, registration command, or source permission change is required. Empty directories are watched until files arrive. UID/GID `10001:10001` needs read access to the files.

Mount `/data` from **one** real bind mount, not an assembled directory of several mounts under an unmounted parent. KeepSync's default identity check is tied to that directory's own device/inode, which stays stable only for a genuine bind mount; a synthesized parent directory gets a new one on every container recreation, including routine image updates. If you need to back up multiple host paths, run one KeepSync container per path instead of combining them under a shared `/data`.

Preserve `keepsync-state` across upgrades and use a dedicated target prefix with one active writer. Saved identities are checked on every backup; a missing state volume does not silently claim an existing backup target. If you lose `keepsync-state` but want to keep backing up to the same prefix, run `docker compose run --rm keepsync reclaim` once to rebuild local state from the existing target, then start normally. If the remote identity marker is gone too, `backup`/`snapshot` recreate it automatically from local state; if both are gone, `keepsync reclaim --force` explicitly claims the target as yours, since nothing left on disk or in S3 can verify that automatically.

Live changes upload after 2 seconds of quiet, or at most 30 seconds of batching during continuous writes. File edits, atomic saves, creates, renames, and deletes are watched recursively. Network/FUSE mounts automatically use a lightweight metadata-polling fallback; set `KEEPSYNC_WATCH_MODE=poll` if your host doesn't deliver filesystem events reliably. A full reconciliation still runs every hour.

Snapshots run on startup and every 30 minutes after completion. Retention keeps **7 recovery points, not 7 files**, and never cleans up `Live/`. Local deletions are logged but stay in `Live/` unless `KEEPSYNC_DELETE_REMOTE=true`.

## Recover your data

### Download and extract a TAR snapshot

Snapshots are stored as a flat pair:

```text
snapshots/20261021T083000.123456789Z-a1b2c3d4e5f67890.tar.gz
snapshots/20261021T083000.123456789Z-a1b2c3d4e5f67890.json
```

The JSON contains source information, creation time, size, and SHA-256. Retention removes both files together. Download the TAR from your S3 console and open it with 7-Zip or extract it with:

```bash
mkdir -p recovery
tar -xzf snapshot.tar.gz -C recovery
```

Your readable, editable files are in `recovery/data/`. The archive includes `keepsync.json` metadata too, so downloading the TAR alone is enough. No KeepSync installation or password is required. Older nested archives remain readable and expire through normal retention.

### Restore through the container

List recovery points, then restore into a new or empty directory writable by the container user:

```bash
docker compose run --rm keepsync snapshots
docker compose run --rm --volume ./recovery:/restore keepsync \
  restore --snapshot <filename.tar.gz> --target /restore/recovered
```

Files appear under `./recovery/recovered/data`. KeepSync verifies the archive before exposing the recovered directory and never restores over the live source.

## Encrypted snapshots and advanced settings

Use [`compose.advanced.yaml`](compose.advanced.yaml) for encrypted Restic history, exclusions, S3-compatible endpoints, JSON logs, and resource settings. [The advanced guide](docs/advanced.md) covers password creation, initialization, recovery without local state, and the full environment reference.

TAR snapshots are full archives; Restic snapshots deduplicate data. Both stream through disk-backed working files rather than loading the dataset into memory. Allow enough state-volume space for a full compressed TAR snapshot. Back up databases using native dumps or consistent filesystem snapshots.

## Everyday commands

```bash
docker compose logs -f keepsync
docker compose run --rm keepsync status
docker compose run --rm keepsync backup     # Update Live now
docker compose run --rm keepsync snapshot   # Save a recovery point now
docker compose run --rm keepsync check      # Verify snapshot data
```

Text logs are short and colored:

```text
12:10:00 WATCH   /data · live · quiet 2s
12:10:04 EDIT    file.json · 1.1K → 5.4K uploaded
12:10:08 ADD     folder/new.json · 80B uploaded
12:10:12 DEL     old.json · kept on S3
12:30:00 SNAP    20261021T123000.000000000Z-a1b2c3d4e5f67890.tar.gz · 24K
```

Use `KEEPSYNC_LOG_COLOR=false` or `NO_COLOR=1` to disable colors, `KEEPSYNC_LOG_FORMAT=json` for structured logs, or `KEEPSYNC_LOG_FILES=false` for batch summaries.

The runtime uses a minimal non-root `scratch` image. Its scheduler sleeps between events, its index lives on disk, and uploads run one file at a time. CI enforces a 40 MiB unpacked image budget.

[Docker Hub](https://hub.docker.com/r/mayankfawkes/keepsync) · [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md) · [MIT License](LICENSE)
