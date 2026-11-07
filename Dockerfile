# syntax=docker/dockerfile:1
# Multi-arch: Go cross-compiles on the build machine's own platform
# (no emulation), only the small runtime stage is per target platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Empty: the version in cmd/bfbackup/main.go. Set it only to override.
ARG VERSION=
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w ${VERSION:+-X main.version=${VERSION}}" -o /out/bfbackup ./cmd/bfbackup

FROM alpine:3.22
ARG VERSION=
LABEL org.opencontainers.image.title="BFBackup" \
      org.opencontainers.image.description="Incremental backups between Unraid (or any Linux) servers, stored as plain files" \
      org.opencontainers.image.url="https://github.com/delsol-ger/bfbackup" \
      org.opencontainers.image.source="https://github.com/delsol-ger/bfbackup" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      net.unraid.docker.icon="https://raw.githubusercontent.com/delsol-ger/bfbackup/main/CI/icon/icon-512.png"
# tini reaps hook child processes; tzdata makes TZ (set by Unraid) work for schedules.
RUN apk add --no-cache tini tzdata ca-certificates
COPY --from=build /out/bfbackup /usr/local/bin/bfbackup
ENV BFB_DATA=/config \
    BFB_ROOTS=/mnt
VOLUME /config
# 7700: web UI (controller), 7701: peer connections (controller)
EXPOSE 7700 7701
ENTRYPOINT ["/sbin/tini", "--", "bfbackup"]
