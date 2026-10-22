# KeepSync

**Automatic S3 backups for Docker: live files and portable TAR snapshots.**

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Mount a folder, set your S3 target and credentials, and start KeepSync. Setup is
automatic, the source stays read-only, and restarts resume backups.

## Quick start

Add this service to your application's Compose file. Replace `./data` with the
existing host folder you want to back up:

```yaml
services:
  keepsync:
    image: mayankfawkes/keepsync:0.4.0
    restart: unless-stopped
    environment:
      KEEPSYNC_TARGET: s3://your-bucket/backups/app
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

Set the credentials in your project's `.env`, then run:

```bash
docker compose up -d keepsync
docker compose logs -f keepsync
```

The image supports Linux `amd64` and `arm64`. The default user `10001:10001`
needs read access to your source; set `user: "UID:GID"` to match the application
when needed. The state volume must be writable by that user.

For a standalone setup, use [`compose.yaml`](compose.yaml) and
[`.env.example`](.env.example).

## What runs automatically

| Behavior | Default |
|---|---|
| Local changes | Upload after 2 seconds of quiet; maximum batching delay 30 seconds |
| Remote verification | Full scan every hour; missing Live objects are re-uploaded from local files |
| Snapshots | Full `.tar.gz` on startup when due, then every 24 hours after completion |
| Retention | Keep 14 successful recovery points |
| Local deletions | Keep the existing S3 copy |
| Empty source | Wait for files without sweeping the remote target |

Live uploads are ordinary objects under `Live/`. Snapshots and their JSON
metadata are stored under `snapshots/`. KeepSync does not encrypt either;
storage-provider encryption is separate. Each TAR snapshot is a full archive,
so allow enough state-volume space to stage it. For databases, back up native
dumps or a consistent filesystem snapshot rather than relying on copies of
actively changing database files.

To change the schedule, add only the settings you need:

```yaml
environment:
  KEEPSYNC_INTERVAL: 10s             # Check S3 for missing Live files
  KEEPSYNC_SNAPSHOT_INTERVAL: 24h
  KEEPSYNC_SNAPSHOT_RETAIN: "14"
  # KEEPSYNC_S3_ENDPOINT: https://s3.example.com
```

A `10s` interval increases S3 requests: each full scan checks remote metadata
for eligible local files. It is separate from live-change detection and the
snapshot schedule. Disable new snapshots with `KEEPSYNC_SNAPSHOTS_ENABLED=false`.

## Keep upgrades simple

Preserve the state volume and use a dedicated target prefix with one active
writer. Mount the source directory itself at `/data`. To include `data/` and
`config/` in one backup, place both inside a real host directory such as
`./source/` and mount `./source:/data:ro`. Mounting only children under an
unmounted `/data` parent can cause identity errors when containers are recreated.

```bash
docker compose pull keepsync
docker compose up -d keepsync
```

A deleted remote identity marker is repaired from intact local state. If the
state volume was lost, stop the sidecar and explicitly reconnect it:

```bash
docker compose stop keepsync
docker compose run --rm keepsync reclaim
docker compose up -d keepsync
```

If both identities are gone, `reclaim --force` explicitly claims the target.
Use it only when you have verified that the mounted source belongs to that
backup prefix. KeepSync never makes that decision automatically.

## Restore

Download a `.tar.gz` from `snapshots/` and extract it with any TAR/GZIP tool:

```bash
mkdir -p recovery
tar -xzf snapshot.tar.gz -C recovery
```

Your files are in `recovery/data/`; `keepsync.json` contains recovery metadata.
No KeepSync installation is required.

Or list snapshots and restore through KeepSync into a new directory writable
by the container user:

```bash
docker compose run --rm keepsync snapshots
docker compose run --rm --volume ./recovery:/restore keepsync \
  restore --snapshot <filename.tar.gz> --target /restore/recovered
```

Files appear in `./recovery/recovered/data/`. Restore is always explicit,
verifies the archive, and never overwrites the live source.

## Useful commands

```bash
docker compose run --rm keepsync status
docker compose run --rm keepsync backup      # Verify and update Live now
docker compose run --rm keepsync snapshot    # Save a recovery point now
docker compose run --rm keepsync check       # Verify snapshot archives
```

## Upgrading from 0.3.x

Version 0.4.0 removes Restic and password-based snapshots. Existing Live data,
TAR history, and source registrations remain usable. Remove password settings
and select TAR if your old configuration explicitly selects another format.
Existing encrypted repositories are neither migrated nor deleted; use a
separately installed Restic with the original password to recover them.

[Advanced configuration and recovery](docs/advanced.md) ·
[Docker Hub](https://hub.docker.com/r/mayankfawkes/keepsync) ·
[Changelog](CHANGELOG.md) · [Contributing](CONTRIBUTING.md) ·
[Security](SECURITY.md) · [MIT License](LICENSE)
