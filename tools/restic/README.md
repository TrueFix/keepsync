# Restic build

This module locks the Restic executable and its dependencies independently of
the KeepSync application. Docker and CI use the same trimmed S3/local build,
without unrelated cloud backends, FUSE mounting, or self-update support.

```bash
go run ./scripts/resticbuild -output bin/restic
go run ./scripts/resticbuild -action test
go run ./scripts/resticbuild -action scan
```

Dependabot checks this module weekly. After updating dependencies, run the
upstream tests above and KeepSync's backup/restore integration tests with the
new executable on `PATH`.

Run these commands from the repository root. The helper patches a temporary
copy of the pinned source; the module cache and upstream module files stay intact.
Builds use Restic's default build tags, which exclude its self-update command.
The module-level OpenPGP advisory GO-2026-5932 refers to unused packages in
`golang.org/x/crypto`; those packages are not imported by this build. Source
scans distinguish that advisory from vulnerabilities in the compiled code.
