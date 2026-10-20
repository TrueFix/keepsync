#!/usr/bin/env bash
set -euo pipefail
version="${1:?version required}"
test "$version" = "$(tr -d '\n' < VERSION)"
mkdir -p dist
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  name="keepsync_${version}_${os}_${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "dist/$name" ./cmd/keepsync
done
(cd dist && sha256sum keepsync_* > SHA256SUMS && sha256sum --check --strict SHA256SUMS)
