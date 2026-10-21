#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
helper="alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
root="$(mktemp -d)"
trap 'docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -c "rm -rf /work/*"; rmdir "$root"' EXIT
mkdir -p "$root"/{data,state,repo,restore,secrets}
printf 'recover me\n' > "$root/data/example.txt"
printf 'test-password\n' > "$root/secrets/password"
docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -c 'chmod 0755 /work; chown -R 10001:10001 /work/*'
run() {
  local format="${SNAPSHOT_FORMAT:-restic}"
  local -a password_mount=()
  if [ "$format" = restic ]; then
    password_mount=(-v "$root/secrets/password:/run/secrets/restic_password:ro")
  fi
  docker run --rm --read-only --tmpfs /tmp:mode=1777 --tmpfs /cache:mode=1777 \
    -e KEEPSYNC_DATASET=smoke -e KEEPSYNC_TARGET=/repo -e KEEPSYNC_SNAPSHOTS_ENABLED=true \
    -e "KEEPSYNC_SNAPSHOT_FORMAT=$format" \
    -v "$root/data:/data:${SOURCE_MODE:-ro}" \
    -v "$root/state:/var/lib/keepsync" -v "$root/repo:/repo" \
    -v "$root/restore:/restore" "${password_mount[@]}" \
    "$image" "$@"
}
if run backup; then
  exit 1
fi
run init
SOURCE_MODE=rw run register-source
run backup
run snapshot
run check
snapshot="$(docker run --rm --entrypoint restic -v "$root:/work" "$image" --repo /work/repo/Snapshots --password-file /work/secrets/password snapshots --json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
run restore --snapshot "$snapshot" --target /restore/recovered
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -c 'cmp /work/data/example.txt /work/restore/recovered/data/example.txt && cmp /work/data/example.txt /work/repo/Live/example.txt'

# Portable snapshots must work without mounting any password file.
SNAPSHOT_FORMAT=tar run snapshot
SNAPSHOT_FORMAT=tar run check
tar_snapshot="$(SNAPSHOT_FORMAT=tar run status | python3 -c 'import json,sys; print(json.load(sys.stdin)["snapshot_id"])')"
tar_key="$(SNAPSHOT_FORMAT=tar run snapshots | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["key"])')"
SNAPSHOT_FORMAT=tar run restore --snapshot "$tar_snapshot" --target /restore/tar-recovered
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -c \
  'mkdir -p /work/manual; tar -xzf "/work/repo/$1" -C /work/manual; cmp /work/data/example.txt /work/manual/data/example.txt && cmp /work/data/example.txt /work/restore/tar-recovered/data/example.txt' _ "$tar_key"
run check
