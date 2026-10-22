# Advanced configuration and recovery

KeepSync 0.4.0 uses one snapshot engine: portable `.tar.gz` archives. Live mode
and daily snapshots are enabled by default; no initialization or password is
required. [`compose.advanced.yaml`](../compose.advanced.yaml) exposes optional
controls. Use it on its own, not as an override:

```bash
docker compose -f compose.advanced.yaml up -d keepsync
```

## Configuration reference

| Variable | Default | Purpose |
|---|---|---|
| `KEEPSYNC_TARGET` | Required | `s3://bucket/prefix`; absolute paths also work for local testing |
| `KEEPSYNC_DATASET` | `files` | Stable dataset name |
| `KEEPSYNC_SOURCE_PATH` | `/data` | Source inside the container |
| `KEEPSYNC_STATE_DIR` | `/var/lib/keepsync` | Persistent identity, index, status, and staging |
| `KEEPSYNC_RESTORE_ROOT` | `/restore` | Parent of explicit restore destinations |
| `KEEPSYNC_METHOD` | `live` | `live` for watched changes; `interval` for scheduled scans only |
| `KEEPSYNC_INTERVAL` | `1h` | Full local/remote reconciliation schedule |
| `KEEPSYNC_DEBOUNCE` | `2s` | Quiet period before a live batch |
| `KEEPSYNC_MAX_WAIT` | `30s` | Maximum batching delay during continuous changes |
| `KEEPSYNC_WATCH_MODE` | `auto` | `native`, `poll`, or automatic filesystem selection |
| `KEEPSYNC_WATCH_INTERVAL` | `5s` | Metadata polling interval |
| `KEEPSYNC_MAX_WATCHES` | `8192` | Directory-watch limit before polling fallback |
| `KEEPSYNC_SNAPSHOTS_ENABLED` | `true` | Create scheduled snapshots |
| `KEEPSYNC_SNAPSHOT_INTERVAL` | `24h` | Delay after the last successful snapshot |
| `KEEPSYNC_SNAPSHOT_RETAIN` | `14` | Successful recovery points to keep |
| `KEEPSYNC_DELETE_REMOTE` | `false` | Delete tracked Live objects when local files disappear |
| `KEEPSYNC_EXCLUDES` | None | JSON array, e.g. `["**/.cache/**","**/node_modules/**"]` |
| `KEEPSYNC_S3_ENDPOINT` | None | S3-compatible HTTP(S) origin without bucket/path |
| `KEEPSYNC_LOG_FORMAT` | `text` | `text` or `json` |
| `KEEPSYNC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `KEEPSYNC_LOG_FILES` | `true` | Per-file upload logs; false gives batch summaries |
| `KEEPSYNC_LOG_COLOR` | `true` | Colored text; `NO_COLOR` also disables it |

Standard AWS credential variables, including `AWS_SESSION_TOKEN`, are supported.
Set `AWS_DEFAULT_REGION` for the provider. `KEEPSYNC_SNAPSHOT_FORMAT=tar` is
accepted for existing deployments but is unnecessary; other formats are rejected.

The minimal Compose file exposes common settings; the advanced file exposes
watcher, exclusion, logging, and resource controls too. Environment variables
must be passed into the container, not merely added to a host `.env` unless the
Compose file references them.

### S3-compatible storage

```yaml
environment:
  KEEPSYNC_TARGET: s3://your-bucket/backups/app
  KEEPSYNC_S3_ENDPOINT: https://region.storage.example.com
  AWS_DEFAULT_REGION: your-region
```

### Reconciliation and deletion

Live events watch local files. Remote changes are discovered during a full scan
at startup or every `KEEPSYNC_INTERVAL`; unchanged files are checked with S3
`HEAD` requests. A missing object is re-uploaded if its local source still exists.
`10s` gives frequent repair scans at the cost of more requests. This does not
change the snapshot schedule or the local-event debounce.

With `KEEPSYNC_DELETE_REMOTE=false`, local deletions are logged and their S3
copies are retained. Enable it to mirror tracked deletions. Only paths in the
persisted index can be removed; an empty full scan never becomes a deletion sweep.
KeepSync does not synchronize remote changes back into the local source or
automatically delete unknown remote objects.

Native watching handles creates, edits, atomic saves, renames, and removals
recursively. Network/FUSE/9P filesystems and watch-limit exhaustion fall back to
metadata polling. Set `KEEPSYNC_WATCH_MODE=poll` if events are unreliable.

## Multiple folders in one backup

Use one persistent host directory containing the folders:

```text
app/
  compose.yaml
  source/
    data/
    config/
```

The application can mount each child separately while KeepSync mounts the parent:

```yaml
# Application service:
volumes:
  - ./source/data:/app/data
  - ./source/config:/app/config

# KeepSync service:
volumes:
  - ./source:/data:ro
  - state:/var/lib/keepsync
```

KeepSync's automatic source identity records the source directory's device/inode.
Mounting only `/data/data` and `/data/config` leaves `/data` in the container
filesystem, where its identity can change on recreation. The symptom is
`source volume changed; existing backup identity preserved`. A real parent bind
mount avoids this. Named source volumes also provide a persistent source root.
Keep state outside the source, and keep its volume name stable across upgrades.

If separate sidecars are needed, give each its own target and state volume.
Omit `container_name` to let Compose generate project-scoped names.

## Identity recovery

| Missing state | Action |
|---|---|
| Remote marker only | Recreated automatically from trusted local registration |
| Local state only | Stop the sidecar, run `reclaim`, start again |
| Local state and remote marker | Explicit `reclaim --force` after verifying ownership |

`reclaim` refuses an existing registration and never runs automatically. Do not
delete healthy state to fix a misconfigured source mount. A changed source
directory is accepted only with the matching recovery identity marker.

TAR archives include the source identity for recovery. Extracted files live under
`data/`, including `.keepsync-source-id`. KeepSync can verify that marker when
reconnecting recovered data. Preserve the original state whenever possible.

## Archives, retention, and restores

New archives are flat pairs under `snapshots/`:

```text
snapshots/<UTC timestamp>-<random>.tar.gz
snapshots/<UTC timestamp>-<random>.json
```

Metadata records the dataset, source identity, creation time, compressed size,
and SHA-256. JSON is published last; incomplete pairs are not recovery points.
The TAR also contains `keepsync.json`, so it can be recovered independently.
Older nested TAR history remains readable and eligible for retention.

Retention counts recovery points, removes matching archive/metadata pairs, and
runs only after a successful snapshot. It never prunes Live files or another
source's snapshots. `prune` requires a successful current-format snapshot status.

TAR preserves regular permissions and safe relative symlinks. Special files and
unsafe links fail the snapshot. Restore rejects traversal, hard links, duplicate
files, symlink traversal, and corrupt archives. It verifies in a temporary
directory before publishing the destination, and requires a new/empty directory
under `KEEPSYNC_RESTORE_ROOT` outside source/state paths.

```bash
docker compose run --rm keepsync snapshots
docker compose run --rm keepsync check
docker compose run --rm --volume ./recovery:/restore keepsync \
  restore --snapshot <exact-filename.tar.gz> --target /restore/recovered
```

Existing snapshots remain listable, checkable, and restorable with new snapshots
disabled. If the machine/state is lost, download a TAR and extract it directly;
manual extraction requires no local registration.

## Resources and database backups

The non-root scratch image contains the KeepSync executable and TLS roots.
`GOMEMLIMIT=64MiB` is a soft Go heap target; Compose's `mem_limit` is the hard
process limit. Uploads stage one file at a time. Snapshots/downloads use disk
staging: allow room for a complete compressed archive and restored files.
No-op scans are quiet; use debug logging for scan diagnostics.

File stabilization is not a transaction across database files. SQLite database
and WAL copies taken at different times may be inconsistent. Use the database's
native backup/dump facility or a consistent filesystem snapshot, and point
KeepSync at the resulting backup files when transactional recovery is required.

## JSON configuration

Mount [`config.example.json`](../config.example.json) and select it explicitly:

```yaml
command: ["--config", "/etc/keepsync/config.json", "serve"]
```

Pass `--config /etc/keepsync/config.json` before management commands too. JSON
replaces KeepSync environment configuration; AWS credentials and runtime memory
settings still use their standard environment variables. Unknown fields fail
validation.

## Upgrade from encrypted snapshots

0.4.0 removes the encrypted snapshot engine, bundled binary, password settings,
and repository initialization. Remove `password_file` and `restic` from old JSON
configuration; remove password mounts/environment variables from Compose. Omit
the snapshot format or set it to `tar`. Keep your existing source/state/target.

Old encrypted repositories are left untouched and are not converted. Recover
them using a separately installed Restic and the original password. Do not
delete that history or its password until you have verified your replacement
TAR recovery points. Historical changelog entries describe older releases.
