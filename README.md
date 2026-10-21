# KeepSync

**Live S3 backups with snapshot history, built for Docker.**

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Mount your files at `/data`. KeepSync uploads changes to `Live/` and saves scheduled recovery points under `Snapshots/`. **Restarts resume backups; restores are always explicit.**

| Snapshot format | Password | Recovery |
|---|---|---|
| `tar` | None; unencrypted `.tar.gz` | Download and extract on your PC |
| `restic` | Required; encrypted and deduplicated | Restore with KeepSync or Restic |

`Live/` contains ordinary files. Your storage provider's server-side encryption is separate from snapshot encryption.

## Live + TAR snapshots every 30 minutes

Use the [Docker Hub image](https://hub.docker.com/r/mayankfawkes/keepsync) for Linux `amd64` or `arm64`. TAR snapshots require version `0.3.0` or later.

```yaml
services:
  keepsync:
    image: mayankfawkes/keepsync:0.3.0
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

Set your target and credentials, then start your application so `./data` contains files. UID/GID `10001:10001` needs read access to the data and write access once for registration:

```bash
docker compose pull keepsync
docker compose run --rm --volume ./data:/data:rw keepsync register-source
docker compose up -d keepsync
```

No password or `init` command is needed for TAR snapshots. Keep the state volume and `.keepsync-source-id` marker across upgrades. Use a dedicated target prefix and one active writer per dataset.

Live changes are batched after 30 seconds of quiet, with a periodic reconciliation every hour. Snapshots run on startup and every 30 minutes after completion. Retention keeps **7 recovery points, not 7 files**, and never cleans up `Live/`. Local deletions stay in `Live/` unless `KEEPSYNC_DELETE_REMOTE=true`.

## Recover your data

### Download and extract a TAR snapshot

Download a `.tar.gz` object from `Snapshots/tar/<dataset>/<source-id>/` using your S3 console. Open it with 7-Zip or extract it with:

```bash
mkdir -p recovery
tar -xzf snapshot.tar.gz -C recovery
```

Your readable, editable files are in `recovery/data/`. `keepsync.json` contains backup metadata. No KeepSync installation or password is required.

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

The runtime uses a minimal non-root `scratch` image. Its scheduler sleeps between events, its index lives on disk, and uploads run one file at a time. CI enforces a 40 MiB unpacked image budget.

[Docker Hub](https://hub.docker.com/r/mayankfawkes/keepsync) · [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md) · [MIT License](LICENSE)
