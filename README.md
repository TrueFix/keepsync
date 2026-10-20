# KeepSync 🐳☁️

[![CI](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/ci.yml)
[![Security](https://github.com/TrueFix/keepsync/actions/workflows/security.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/security.yml)
[![Release](https://github.com/TrueFix/keepsync/actions/workflows/release.yml/badge.svg)](https://github.com/TrueFix/keepsync/actions/workflows/release.yml)

**A lightweight, Docker-ready backup service built in Go. Back up local files to S3 with scheduled snapshots, explicit restores, and restart-safe safeguards. 🔒**

## The restart rule

**Restarting resumes local → S3 backups. Restoring S3 → local always requires an explicit command.**

KeepSync never guesses a sync direction, restores on startup, or mirrors deletions over your live files. Each successful backup creates a historical snapshot using [Restic](https://restic.net/), which supplies encryption, deduplication, repository locking, and integrity verification.

| Event | Behavior |
|---|---|
| Normal restart | Validate identities and immediately attempt a new backup |
| Interrupted backup | Previous completed snapshots remain available; next run retries |
| Missing source marker, wrong volume, or empty source | Block backup and record an error |
| Missing repository or changed repository identity | Block; never silently initialize |
| S3 outage | Leave source untouched; retry after the configured interval |
| Lost source disk | Explicitly restore a chosen snapshot into a separate directory |
| Local deletion | Preserved in older snapshots until explicit retention removes them |

## Features

- 🐹 Small Go controller with no third-party Go dependencies.
- 🐳 Non-root Docker image; Linux amd64/arm64 container releases.
- ☁️ Amazon S3 and S3-compatible storage through Restic.
- 🔐 Encrypted, incremental, deduplicated snapshots.
- 🛡️ Persistent source/repository identity checks and read-only backup mounts.
- 📦 Linux and macOS amd64/arm64 CLI releases with SHA-256 checksums.
- 🔄 Configurable interval, explicit snapshot restores with verification, and opt-in retention.
- 🧪 Real backup/recovery integration tests, race detection, security scans, and automated releases.

## Docker quick start

### 1. Prepare configuration and secrets

```bash
cp config.example.json config.json
cp .env.example .env
mkdir -p secrets recovery
```

Edit `config.json`: set the S3 bucket/prefix and dataset name. Edit `.env`: set the **existing absolute** source path, AWS credentials, and recovery path. Place a strong repository password in `secrets/restic_password`. Keep a separate recovery copy of that password. Neither credentials nor the password belong in Git or the image.

The default container runs as UID/GID `10001:10001`. It needs read access to source files and the password, and write access to state/cache volumes and recovery directories. The setup-only registration container also needs permission to create a marker in the source directory. Arrange ownership or ACLs for your deployment; do not make secrets world-readable. Alternatively, configure the Compose `user` to a suitable non-root host UID/GID and give it matching volume permissions.

### 2. Build and explicitly initialize

```bash
docker compose build
docker compose run --rm keepsync init
docker compose --profile setup run --rm register
```

`init` creates a repository and must only be used for a new repository. `register-source` creates `.keepsync-source-id` in your source and binds its identity and repository ID to the persistent state volume. An existing marker can be reused when explicitly registering a recovery, but an existing registration is never silently replaced.

Do not put the source on an unmounted fallback directory. The host bind mount must already exist, and the identity marker must remain on the actual source volume.

### 3. Start backups

```bash
docker compose up -d keepsync
docker compose logs -f keepsync
docker compose run --rm keepsync status
docker compose run --rm keepsync snapshots
```

The source is read-only in the normal backup container. A backup is attempted immediately on startup and then after each configured interval, measured from the previous attempt's completion. Failed attempts retry on the same interval. SIGTERM cancels the active Restic process; only a successfully completed run updates `last_success`.

`status` prints the last attempt, last successful backup, and last error. A process that is running can still have blocked backups: monitor **last successful backup age**, not just container uptime. Status reports backup history and does not itself probe S3.

State must be persistent and stored on a local filesystem supporting POSIX locks. Do not remove the state volume during normal upgrades (`docker compose down -v` deletes it). Use one active backup writer per dataset; the local lock only coordinates processes sharing the same state volume.

### 4. Restore deliberately

Create the configured host recovery directory first and make it writable by the container user. Choose an explicit snapshot ID from `snapshots`:

```bash
docker compose --profile recovery run --rm restore \
  restore --snapshot <snapshot-id> --target /restore/recovery-2026-10-20
```

The destination must be a new or empty child of `restore_root`. Restores into the source, state, non-empty directories, or another dataset's snapshot are rejected. `latest` is deliberately rejected; use a hexadecimal snapshot ID. Restic preserves the original absolute directory hierarchy beneath the target: backing up `/data` restores files beneath `/restore/recovery-2026-10-20/data`.

Review recovered files before promoting them to live data. If the source disk is gone, create an empty source mount point for Compose; restore does not require the source marker or files. To register recovered data, stop the service, promote the reviewed directory, then explicitly run registration with a **new state volume**. Keep the old state until recovery is verified.

### 5. Verify and manage retention

```bash
docker compose run --rm keepsync check
```

`check` reads and verifies all repository data. This can incur S3 transfer/request charges. Periodically perform a sample restore as well.

Retention defaults to disabled. After verifying recovery, enable it and choose keep counts in `config.json`. Then run it deliberately:

```bash
docker compose run --rm keepsync prune
```

`prune` requires that the last recorded backup succeeded. It expires snapshots tagged for this dataset according to the daily/weekly/monthly policy, then asks Restic to reclaim unreferenced repository data. It never runs from `serve`. Deleting a backup is permanent unless you have independent storage-level recovery. Prefer a separate repository prefix per dataset.

## Configuration

Configuration is strict JSON; unknown fields are rejected. Use `--config FILE` **before** the command:

```bash
keepsync --config /path/to/config.json backup
```

See [`config.example.json`](config.example.json). Paths must be absolute, non-root, and keep source, state, and restore directories separate. Repository URLs support local paths for testing, AWS S3, and Restic-supported S3-compatible endpoints. AWS credentials use the standard environment variables or workload identity/instance-role mechanisms supported by Restic. `RESTIC_*` environment variables are intentionally not inherited: repository and password selection come from configuration.

| Setting | Meaning |
|---|---|
| `dataset` | Stable alphanumeric/hyphen/underscore identity, up to 64 characters |
| `source` | Existing directory to protect |
| `repository` | Restic repository URL, e.g. `s3:s3.amazonaws.com/bucket/prefix` |
| `password_file` | Repository encryption password file |
| `state_dir` | Persistent registration, operation lock, and backup status |
| `restore_root` | Parent directory allowed for explicit restores |
| `interval` | Go duration of at least 1s; default `1h` |
| `restic` | Restic executable; default `restic` |
| `excludes` | Restic exclusion patterns |
| `retention` | Disabled by default; daily/weekly/monthly keep counts |

## Local development

Requires Go 1.26+, Restic 0.16+, and a POSIX system (Linux or macOS). Windows users can use Docker/WSL; native Windows binaries are not published.

```bash
make build
make check
KEEPSYNC_REQUIRE_INTEGRATION=1 make check
make release
```

Without Restic, integration tests explicitly skip; CI requires them. Tests use isolated local encrypted repositories and verify backups, restart behavior, older-snapshot recovery, missing/empty sources, identity mismatches, cross-dataset isolation, and non-destructive restore guards. CI also tests the container with an actual backup and restore. Live AWS credentials are not required for these tests; validate your bucket permissions and recovery procedure in your deployment.

## Versioning and releases 🚀

`VERSION` is the single release version source. The CLI gets its version at build time. Releases follow Semantic Versioning; pre-1.0 versions may introduce breaking configuration changes, which should be documented in the changelog.

1. Update `VERSION`, the default Compose version and `.env.example`, and `CHANGELOG.md` in a reviewed commit.
2. Wait for CI and security checks to pass on `main`.
3. Tag that commit and push the tag:

   ```bash
   git tag -a v0.1.0 -m "KeepSync v0.1.0"
   git push origin v0.1.0
   ```

**Every tag push triggers the release workflow.** Publication requires a valid `vMAJOR.MINOR.PATCH` or prerelease tag matching `VERSION`; arbitrary/mismatched tags fail validation. A manual release run accepts an existing tag. Tests and vulnerability checks run before publication.

Published artifacts:

- Linux/macOS amd64 and arm64 binaries, `SHA256SUMS`, and build provenance attestations.
- `ghcr.io/truefix/keepsync:<version>` for Linux amd64/arm64, with SBOM and provenance.
- The `latest` container tag for non-prerelease releases only. Publish stable releases in increasing version order; publishing an older stable tag also updates `latest`.

Use immutable version tags (or image digests) in deployments. Never move or reuse a published version tag. The workflow publishes containers before the GitHub release; if publication is partially interrupted, inspect existing artifacts before retrying.

## GitHub automation

| Automation | Purpose |
|---|---|
| CI | Linux/macOS format, vet, race/recovery tests, versioned build, Docker smoke tests, workflow/shell lint, Go vulnerability scan |
| Release | Every tag push, version validation, tested binaries, checksums, GHCR multi-arch image, SBOM, provenance, generated release notes |
| Security | CodeQL, fixed HIGH/CRITICAL container vulnerability scan, PR dependency review; weekly and on main/PRs |
| Dependabot | Weekly Go, Docker, and GitHub Actions update PRs |

Maintainers should enable branch protection for `main`, require passing checks/review, and restrict creation/deletion of release tags with GitHub rulesets. CodeQL, dependency review, and artifact attestations require the corresponding GitHub features to be available (public repositories support them). Publishing uses the workflow's `GITHUB_TOKEN`, with package write permission limited to the publishing job. Initial GHCR packages may need their visibility set to public in GitHub package settings.

## Current scope

KeepSync is a small backup controller, not a two-way sync engine. It does not detect arbitrary corruption, ransomware, all non-empty volume substitutions, or mass deletions that leave some files behind. The source marker, history, and explicit retention reduce risk; they cannot prove the source contains the data you intended. Do not prune aggressively. Back up databases using native dumps or consistent filesystem snapshots rather than copying changing database files. S3 versioning and independent recovery credentials can provide additional protection.

## License

[MIT](LICENSE). Restic is a separate [BSD-2-Clause licensed program](https://github.com/restic/restic/blob/master/LICENSE); the Docker image includes its packaged executable.
