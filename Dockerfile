# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/keepsync ./cmd/keepsync

FROM alpine:3.23
RUN apk add --no-cache ca-certificates restic tzdata && \
    addgroup -g 10001 keepsync && adduser -D -u 10001 -G keepsync keepsync && \
    mkdir -p /var/lib/keepsync /restore /cache && chown -R keepsync:keepsync /var/lib/keepsync /restore /cache
COPY --from=build /out/keepsync /usr/local/bin/keepsync
COPY LICENSE /usr/share/licenses/keepsync/LICENSE
COPY third_party/restic.LICENSE /usr/share/licenses/restic/LICENSE
ENV XDG_CACHE_HOME=/cache
USER 10001:10001
ENTRYPOINT ["keepsync"]
CMD ["serve"]
