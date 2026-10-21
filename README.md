# KeepSync

**Live S3 backups and optional snapshot history, built for Docker.**

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Mount your application's files at `/data`. KeepSync watches for changes, uploads them to S3, and optionally saves encrypted recovery points on a separate schedule.

```text
s3://your-bucket/backups/app/
├── Live/        Files under their original relative paths
└── Snapshots/   Encrypted, deduplicated Restic history
```

**Restarts resume backups. Restores are always explicit.** `Live/` uses your bucket's encryption settings; snapshots use client-side encryption through [Restic](https://restic.net/).

## Docker Compose 🐳

```yaml
services:
  keepsync:
    image: mayankfawkes/keepsync:0.2.0
    restart: unless-stopped
    read_only: true
    cpus: 0.5
    mem_limit: 256m
    environment:
      KEEPSYNC_TARGET: s3://your-bucket/backups/app
      KEEPSYNC_DATASET: app-files
      KEEPSYNC_METHOD: live
      KEEPSYNC_DEBOUNCE: 30s
      KEEPSYNC_INTERVAL: 1h
      KEEPSYNC_SNAPSHOTS_ENABLED: "true"
      KEEPSYNC_SNAPSHOT_INTERVAL: 24h
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
      - keepsync-cache:/cache
    secrets:
      - restic_password
    tmpfs:
      - /tmp:mode=1777,size=16m

secrets:
  restic_password:
    file: ./secrets/restic_password

volumes:
  keepsync-state:
  keepsync-cache:
```

Images are published on [Docker Hub](https://hub.docker.com/r/mayankfawkes/keepsync) for Linux `amd64` and `arm64`. Set your S3 target and AWS credentials in your shell or `.env`. Save a strong snapshot password in `secrets/restic_password` and keep a separate recovery copy.

### First-time setup

Start your application so `./data` contains files. The container runs as UID/GID `10001:10001`: it needs read access to the data/password and write access to the data directory once for registration.

```bash
docker compose pull keepsync
docker compose run --rm keepsync init
docker compose run --rm --volume ./data:/data:rw keepsync register-source
docker compose up -d keepsync
```

Skip `init` for existing snapshot repositories. Preserve the source's `.keepsync-source-id` marker and the state volume across upgrades. Use a dedicated S3 prefix and one active writer per dataset.

**Named volumes:** replace the source bind mount with `your-volume:/data:ro`, keeping state/cache mounts. Register with `--volume your-volume:/data:rw`; declare externally owned volumes as external in Compose.

## How it works

- **Live mode:** batches changes after 30 seconds of quiet. Continuous writes trigger an attempt after `KEEPSYNC_MAX_WAIT` (default `5m`). The interval remains a full reconciliation fallback.
- **Interval mode:** set `KEEPSYNC_METHOD=interval` to back up on startup and after each interval.
- **Snapshots:** run independently of file changes. The example keeps **7 confirmed recovery points**, not 7 individual files. Retention never touches `Live/`.

Set `KEEPSYNC_SNAPSHOTS_ENABLED=false` for Live-only backups; the password and secret mount are then unnecessary. Local deletions stay in `Live/` unless `KEEPSYNC_DELETE_REMOTE=true`. Missing identities and empty sources block backups.

For databases, back up a native dump or consistent filesystem snapshot. Live uploads cover regular files; snapshots also preserve symlinks.

## Commands and logs

```bash
docker compose logs -f keepsync
docker compose run --rm keepsync status
docker compose run --rm keepsync backup
docker compose run --rm keepsync snapshot
docker compose run --rm keepsync snapshots
docker compose run --rm keepsync check
```

Logs show confirmed uploads and size changes:

```text
msg="FILE uploaded" path=FILEA.JSON size_change="1125 -> 5500 B"
msg="Snapshot saved" snapshot=ab12cd34...
msg="Snapshot retention complete" keep_last=7
```

Use `KEEPSYNC_LOG_FORMAT=json` for structured logs or `KEEPSYNC_LOG_FILES=false` for summaries only. `status` reports Live and snapshot results separately.

## Restore

Choose an ID from `snapshots` and create a recovery directory writable by the container user:

```bash
docker compose run --rm --volume ./recovery:/restore keepsync \
  restore --snapshot <snapshot-id> --target /restore/recovered
```

Files appear under `./recovery/recovered/data`. The destination must be new or empty and separate from the live source. Live-only files can be downloaded directly from S3.

## Lightweight by design

Multi-stage builds produce a non-root `scratch` runtime with static executables and TLS roots. The service sleeps between events, keeps its index on disk, and uploads one file at a time. Staging uses disk space on the state volume, not whole-file memory buffers.

A local Linux test with 1,000 small files, one watched directory, and snapshots disabled measured **16.53 MiB idle RAM** and **0.000% CPU over 5 seconds**. Usage varies with workload; large snapshot repositories may need a higher memory limit. CI enforces a **40 MiB unpacked image budget**.

For S3-compatible storage, set `KEEPSYNC_S3_ENDPOINT`. Advanced settings are in [`config.example.json`](config.example.json); select a complete JSON configuration with `--config /path/to/config.json` before each command.

[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md) · [MIT License](LICENSE)
