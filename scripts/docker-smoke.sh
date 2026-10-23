#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
helper="alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
root="$(mktemp -d)"
cid=""
trap 'if [ -n "$cid" ]; then docker rm -f "$cid"; fi; docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -c "rm -rf /work/*"; rmdir "$root"' EXIT
mkdir -p "$root"/{data,state,repo,restore}
printf 'recover me\n' > "$root/data/example.txt"
image_user="$(docker image inspect "$image" --format '{{.Config.User}}')"
case "$image_user" in
  0:0|10001:10001) ;;
  *) printf 'Unexpected image user: %s\n' "$image_user" >&2; exit 1 ;;
esac
docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -c \
  'chmod 0755 /work; chown -R "$1" /work/*; chmod 0700 /work/state; chmod 0600 /work/data/example.txt' _ "$image_user"
run() {
  docker run --rm --read-only --tmpfs /tmp:mode=1777 \
    -e KEEPSYNC_DATASET=smoke -e KEEPSYNC_TARGET=/repo -e KEEPSYNC_SNAPSHOTS_ENABLED=true \
    -v "$root/data:/data:${SOURCE_MODE:-ro}" \
    -v "$root/state:/var/lib/keepsync" -v "$root/repo:/repo" \
    -v "$root/restore:/restore" \
    "$image" "$@"
}

# The public entrypoint must work on a fresh state volume with /data read-only.
start() {
  cid="$(docker run -d --read-only --tmpfs /tmp:mode=1777 \
    -e KEEPSYNC_DATASET=smoke -e KEEPSYNC_TARGET=/repo -e KEEPSYNC_METHOD=live \
    -e KEEPSYNC_DEBOUNCE=1s -e KEEPSYNC_MAX_WAIT=2s -e KEEPSYNC_DELETE_REMOTE=true \
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
# Writes by another container must reach the read-only sidecar mount.
has_content() {
  docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec \
    'test "$(cat "/work/repo/Live/$1")" = "$2"' _ "$1" "$2"
}
wait_content() {
  for _ in {1..30}; do
    if has_content "$1" "$2" 2>/dev/null; then return; fi
    sleep 1
  done
  docker logs "$cid"
  return 1
}
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec 'printf "edited live\n" > /work/data/example.txt; mkdir -p /work/data/new/deep; printf "created live\n" > /work/data/new/deep/file.txt'
wait_content example.txt 'edited live'
wait_content new/deep/file.txt 'created live'
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec 'mv /work/data/new/deep/file.txt /work/data/new/deep/renamed.txt; printf "nested edit\n" > /work/data/new/deep/renamed.txt'
wait_content new/deep/renamed.txt 'nested edit'
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec 'rm -rf /work/data/new'
deleted=false
for _ in {1..30}; do
  if docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec 'test ! -e /work/repo/Live/new/deep/renamed.txt'; then deleted=true; break; fi
  sleep 1
done
test "$deleted" = true
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

# Archives work with automatic defaults and no separate initialization.
run backup
run snapshot
run check

# Rootless must explain root-owned bind-mount failures rather than requiring
# users to guess which UID the image runs as.
if [ "$image_user" = 10001:10001 ]; then
  docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -ec \
    'chown 0:0 /work/state; chmod 0700 /work/state'
  if failure="$(run backup 2>&1)"; then
    printf 'Rootless unexpectedly wrote root-only state\n' >&2
    exit 1
  fi
  case "$failure" in
    *'state directory'*'10001:10001'*) ;;
    *) printf 'Missing permission guidance: %s\n' "$failure" >&2; exit 1 ;;
  esac
  docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$helper" -ec \
    'chown 10001:10001 /work/state'
fi
snapshot="$(run status | python3 -c 'import json,sys; print(json.load(sys.stdin)["snapshot_id"])')"
run restore --snapshot "$snapshot" --target /restore/recovered
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -c 'cmp /work/data/example.txt /work/restore/recovered/data/example.txt && cmp /work/data/example.txt /work/repo/Live/example.txt'

# Archives are also directly readable by standard TAR tools.
tar_snapshot="$snapshot"
tar_key="$(run snapshots | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["key"])')"
case "$tar_key" in
  snapshots/*.tar.gz) ;;
  *) exit 1 ;;
esac
tar_metadata="${tar_key%.tar.gz}.json"
run restore --snapshot "$tar_snapshot" --target /restore/tar-recovered
docker run --rm --entrypoint sh -v "$root:/work" "$helper" -ec \
  'test -f "/work/repo/$2"; mkdir -p /work/manual; tar -xzf "/work/repo/$1" -C /work/manual; cmp /work/data/example.txt /work/manual/data/example.txt; cmp /work/data/example.txt /work/restore/tar-recovered/data/example.txt' _ "$tar_key" "$tar_metadata"
run check
