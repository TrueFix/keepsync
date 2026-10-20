#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
root="$(mktemp -d)"
trap 'docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$image" -c "rm -rf /work/*"; rmdir "$root"' EXIT
mkdir -p "$root"/{data,state,repo,restore,secrets}
printf 'recover me\n' > "$root/data/example.txt"
printf 'test-password\n' > "$root/secrets/password"
cat > "$root/config.json" <<'JSON'
{"dataset":"smoke","source":"/work/data","repository":"/work/repo","password_file":"/work/secrets/password","state_dir":"/work/state","restore_root":"/work/restore","interval":"1h"}
JSON
docker run --rm --user 0 --entrypoint sh -v "$root:/work" "$image" -c 'chmod 0755 /work; chown -R 10001:10001 /work/*'
run() { docker run --rm -v "$root:/work" "$image" --config /work/config.json "$@"; }
run init
run register-source
run backup
run check
snapshot="$(docker run --rm --entrypoint restic -v "$root:/work" "$image" --repo /work/repo --password-file /work/secrets/password snapshots --json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
run restore --snapshot "$snapshot" --target /work/restore/recovered
docker run --rm --entrypoint sh -v "$root:/work" "$image" -c 'cmp /work/data/example.txt /work/restore/recovered/work/data/example.txt'
