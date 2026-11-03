# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS base
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

FROM base AS app-build
ARG VERSION=dev
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$VERSION" -o /out/keepsync ./cmd/keepsync

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS runtime-files
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 keepsync && adduser -D -u 10001 -G keepsync keepsync && \
    mkdir -p /runtime/var/lib/keepsync/tmp /runtime/restore /runtime/tmp /runtime/home/keepsync && \
    chmod 1777 /runtime/tmp

# Only static executables, TLS roots, user records and empty writable directories.
FROM scratch AS runtime-base
COPY --from=runtime-files /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=runtime-files /etc/passwd /etc/group /etc/
COPY --from=runtime-files --chown=10001:10001 /runtime/ /
COPY --from=app-build /out/keepsync /usr/local/bin/keepsync
COPY LICENSE /usr/share/licenses/keepsync/LICENSE
ENV PATH=/usr/local/bin HOME=/home/keepsync TMPDIR=/var/lib/keepsync/tmp \
    GOMAXPROCS=1 GOMEMLIMIT=64MiB
ENTRYPOINT ["/usr/local/bin/keepsync"]
CMD ["serve"]

# Explicit opt-in non-root variant, published as latest-rootless.
FROM runtime-base AS rootless
USER 10001:10001

# Default image supports root-owned host bind mounts without a chown step.
FROM runtime-base AS runtime
COPY --from=runtime-files --chown=0:0 /runtime/ /
USER 0:0
