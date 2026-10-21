#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
helper="alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
root="$(mktemp -d)"
cid=""
trap 'if [ -n "$cid" ]; then docker rm -f "$cid"; fi; docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -c "rm -rf /work/*"; rmdir "$root"' EXIT
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

# The public entrypoint must work on a fresh state volume with /data read-only.
start() {
  cid="$(docker run -d --read-only --tmpfs /tmp:mode=1777 --tmpfs /cache:mode=1777 \
    -e KEEPSYNC_DATASET=smoke -e KEEPSYNC_TARGET=/repo -e KEEPSYNC_METHOD=live \
    -e KEEPSYNC_SNAPSHOTS_ENABLED=true -e KEEPSYNC_SNAPSHOT_FORMAT=tar \
    -v "$root/data:/data:ro" -v "$root/state:/var/lib/keepsync" -v "$root/repo:/repo" \
    "$image")"
}
ready() {
  docker exec "$cid" /usr/local/bin/keepsync status 2>/dev/null | python3 -c '
import json,sys
try:
    status=json.load(sys.stdin)
    success=status.get("last_success", "")
    assert success and not success.startswith("0001") and success != sys.argv[1]
    assert status.get("snapshot_id") and not status.get("last_error") and not status.get("snapshot_error")
except (ValueError, AssertionError):
    sys.exit(1)
' "${previous_success:-}"
}
wait_ready() {
  for _ in {1..60}; do
    if ready; then return; fi
    sleep 1
  done
  docker logs "$cid"
  return 1
}
start
wait_ready
previous_success="$(docker exec "$cid" /usr/local/bin/keepsync status | python3 -c 'import json,sys; print(json.load(sys.stdin)["last_success"])')"
docker stop "$cid"
docker rm "$cid"
cid=""
start
wait_ready
docker stop "$cid"
docker rm "$cid"
cid=""
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -c 'test ! -e /work/data/.keepsync-source-id'

# Encrypted history is initialized automatically when it is first requested.
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
