# Advanced configuration and recovery

The README's configuration uses portable, unencrypted TAR snapshots every 30
minutes. This guide covers encrypted snapshots and additional controls.

The examples use `mayankfawkes/keepsync:0.3.1`. To test a local build instead, run
`make image VERSION=dev` and set `KEEPSYNC_IMAGE=keepsync:test`.

## Encrypted Restic configuration

[`compose.advanced.yaml`](../compose.advanced.yaml) is a complete configuration;
use it by itself with `docker compose -f compose.advanced.yaml ...`.
It enables Live uploads plus encrypted Restic snapshots. Its default schedule is
24 hours with 14 recovery points. Explicit `.env` values override those defaults.

### Set the encryption password

Write your chosen strong password as one line in `secrets/restic_password`, or
generate one on the host:

```bash
mkdir -p secrets
(umask 077; openssl rand -base64 32 > secrets/restic_password)
sudo chown 10001:10001 secrets/restic_password
```

The last command grants the default container user access on Linux without making
the password world-readable. Match ownership to your container user if you change
its UID/GID. Keep an offline copy: **without the password, Restic history cannot
be restored**. Reuse the same file on replacement machines; overwriting the file
does not rotate an existing repository's password.

The advanced Compose file mounts the password as a secret and selects:

```yaml
environment:
  KEEPSYNC_SNAPSHOTS_ENABLED: "true"
  KEEPSYNC_SNAPSHOT_FORMAT: restic
  KEEPSYNC_PASSWORD_FILE: /run/secrets/restic_password
secrets:
  - restic_password
```

The password protects Restic snapshots only. TAR snapshots do not use it, and
Live uploads remain ordinary objects using the bucket's encryption settings.

### Initialize and start

Set `KEEPSYNC_TARGET`, the source path, and AWS credentials in your shell or `.env`.
UID `10001` only needs read access to the source. Startup initializes a fresh
target and its state automatically, keeping the source mount read-only. An empty
source is monitored until the application creates files.

```bash
docker compose -f compose.advanced.yaml up -d keepsync
```

The encrypted repository is initialized automatically when the first snapshot
runs. The mounted password is still required. Existing bound repositories are
verified rather than recreated. Live uploads remain independent of snapshot
password or repository failures.

## Recover encrypted history

### With the state volume available

Mount the original password, use the same target/dataset, and select `restic`.
Create a recovery directory writable by the container user, then:

```bash
docker compose -f compose.advanced.yaml run --rm keepsync snapshots
docker compose -f compose.advanced.yaml run --rm --volume ./recovery:/restore keepsync \
  restore --snapshot <snapshot-id> --target /restore/recovered
```

For the default source, files appear under `./recovery/recovered/data`. Restic
preserves the original source path beneath the destination if you customize it.
Restoring does not require the original source files to exist, but Compose still
requires an existing host directory for its source mount.

### Without the state volume or original machine

Use Restic directly; its repository is self-contained. You need S3 access, the
repository URL, and the original password. Do not initialize a new repository.

With Restic installed on the recovery machine:

```bash
export RESTIC_REPOSITORY=s3:https://s3.amazonaws.com/your-bucket/backups/app/Snapshots
export RESTIC_PASSWORD_FILE=/absolute/path/to/restic_password
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export AWS_DEFAULT_REGION=us-east-1

restic snapshots --tag keepsync:files
restic restore <snapshot-id> --target ./recovery --verify
```

Use `keepsync:<your-dataset>` for the tag. For S3-compatible providers, replace
the endpoint in the repository URL, for example
`s3:https://s3.example.com/bucket/prefix/Snapshots`.

TAR history needs neither local state nor a password: download the archive and
run `tar -xzf snapshot.tar.gz -C ./recovery`. Its `data/` directory contains the
files and a recoverable identity marker. KeepSync's Restic restore adds the same
marker; standalone Restic recovery can obtain the ID from the snapshot's
`keepsync-source:<id>` tag if the recovered data has no marker. Save that ID as
`.keepsync-source-id` in the recovered source before reconnecting to the same
target without the original state. Older manually registered backups already
contain that marker.

Review the recovery and adjust ownership for the application before promoting
it to live data. Preserve the original state volume when returning data to the
same path. KeepSync verifies the restored marker before accepting a replaced
source directory. Ordinary operation never needs to write into the source.

## Choosing a format

| Format | Storage | Password | Recovery |
|---|---|---|---|
| `tar` | Full `.tar.gz` copies under `Snapshots/tar/<dataset>/<source-id>/` | None | Any standard TAR/GZIP extractor, or KeepSync |
| `restic` | Encrypted, deduplicated repository under `Snapshots/` | Required | KeepSync or Restic |

The default format remains `restic` for compatibility; the minimal Compose
example explicitly selects `tar`. Switching formats preserves both histories,
keeps retention separate, and schedules a point in the new format. Select the
matching format when listing, checking, or restoring history. New snapshots can
be disabled while existing history remains restorable.

TAR includes regular files, directories, and relative symlinks that stay inside
`data/`; unsafe links and special files cause the attempt to fail without
expiring older points. KeepSync's TAR restore rejects traversal, hard links,
symlink traversal, duplicate files, and corrupt GZIP data. It restores regular
permissions without privileged mode bits; use Restic where richer filesystem
metadata preservation is required.

## Environment reference

| Variable | Default | Purpose |
|---|---|---|
| `KEEPSYNC_TARGET` | Required | `s3://bucket/prefix`; absolute paths are also supported for local testing |
| `KEEPSYNC_DATASET` | `files` | Stable dataset name |
| `KEEPSYNC_SOURCE_PATH` | `/data` | Source path inside the container |
| `KEEPSYNC_METHOD` | `interval` | `live` or `interval` |
| `KEEPSYNC_INTERVAL` | `1h` | Full reconciliation interval; fallback in Live mode |
| `KEEPSYNC_DEBOUNCE` | `30s` | Quiet period before processing changes |
| `KEEPSYNC_MAX_WAIT` | `5m` | Maximum batching delay during continuous changes |
| `KEEPSYNC_MAX_WATCHES` | `8192` | Directory-watch limit; other directories use reconciliation |
| `KEEPSYNC_SNAPSHOTS_ENABLED` | `false` | Enable scheduled recovery points |
| `KEEPSYNC_SNAPSHOT_FORMAT` | `restic` | `tar` for portable archives, `restic` for encryption/deduplication |
| `KEEPSYNC_SNAPSHOT_INTERVAL` | `24h` | Delay after the last successful snapshot completes |
| `KEEPSYNC_SNAPSHOT_RETAIN` | `7` | Confirmed recovery points retained in the selected format |
| `KEEPSYNC_PASSWORD_FILE` | `/run/secrets/restic_password` | Restic password file; unused by TAR |
| `KEEPSYNC_STATE_DIR` | `/var/lib/keepsync` | Persistent identity, index, status, and transfer staging |
| `KEEPSYNC_RESTORE_ROOT` | `/restore` | Parent for explicit restore destinations |
| `KEEPSYNC_DELETE_REMOTE` | `false` | Remove tracked Live objects when their source files disappear |
| `KEEPSYNC_EXCLUDES` | None | JSON array, e.g. `["**/.cache/**","**/node_modules/**"]` |
| `KEEPSYNC_S3_ENDPOINT` | None | S3-compatible HTTP(S) origin, without a bucket or path |
| `KEEPSYNC_LOG_FORMAT` | `text` | `text` or `json` |
| `KEEPSYNC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `KEEPSYNC_LOG_FILES` | `true` | Log each confirmed Live upload |

Runtime defaults and example limits are distinct: both Compose examples select
Live mode explicitly. The minimal file selects TAR/30m/7; the advanced file selects
Restic/24h/14 unless applicable `.env` values override the schedule or count.

`GOMEMLIMIT` is a soft Go heap target, not a process-memory limit. The image sets
64 MiB for the sidecar and `KEEPSYNC_SNAPSHOT_MEMORY_LIMIT=128MiB` for the Restic
subprocess. `KEEPSYNC_CPUS` and `KEEPSYNC_MEMORY_LIMIT` configure the provided
Compose examples' hard CPU/memory limits. Larger repositories may need a larger
memory limit. TAR creation/downloads use disk-backed staging; allow space for a
complete compressed snapshot and for restored files.

## JSON configuration

For a complete file-based configuration, mount
[`config.example.json`](../config.example.json) and set:

```yaml
command: ["--config", "/etc/keepsync/config.json", "serve"]
```

Pass `--config /etc/keepsync/config.json` before every management command too.
The file replaces KeepSync environment configuration; AWS credentials and the
runtime memory settings still use their standard environment variables.
