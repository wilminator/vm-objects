# syntax=docker/dockerfile:1
#
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.

# Builds vmobj in a container, so the host needs only Docker, not Go.
#
# The release binaries, written to ./bin (no image is kept):
#
#   docker build --target export --output type=local,dest=bin .
#
# A small image that runs vmobj (the default target):
#
#   docker build -t vmobj .
#   docker run --rm vmobj version
#
# Add --build-arg VERSION=1.2.3 to stamp a version into either.
#
# The image suits jobs that only touch files and object storage. Backups
# of VMs and ZFS datasets call the host's virsh and zfs, so run the binary
# directly on the host for those.

ARG GO_VERSION=1.24.7

# --- build ---------------------------------------------------------------
# Runs on the builder's own platform and cross-compiles: vmobj is pure Go,
# so no emulation is needed to produce ARM64 builds on an x86-64 machine.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src

# Dependencies first, so editing the code doesn't download them again.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=0.1.0-dev
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    for arch in amd64 arm64; do \
      GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/vmobj-linux-$arch ./cmd/vmobj || exit 1; \
    done

# The few folders the runtime image needs: a writable /tmp and a home for
# the unprivileged user (credentials files are looked for there).
RUN mkdir -p /rootfs/tmp /rootfs/home/vmobj \
 && chmod 1777 /rootfs/tmp \
 && chown 65532:65532 /rootfs/home/vmobj

# --- export: just the binaries --------------------------------------------
FROM scratch AS export
COPY --from=build /out/ /

# --- runtime: the default target ------------------------------------------
FROM scratch AS runtime
ARG TARGETARCH
ARG VERSION=0.1.0-dev
LABEL org.opencontainers.image.title="vmobj" \
      org.opencontainers.image.description="Block-level, encrypted, incremental VM backups to object storage" \
      org.opencontainers.image.source="https://github.com/wilminator/vm-objects" \
      org.opencontainers.image.licenses="MPL-2.0" \
      org.opencontainers.image.version="${VERSION}"
# Root certificates, for HTTPS to S3 services.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /rootfs/ /
COPY --from=build /out/vmobj-linux-${TARGETARCH} /vmobj
ENV HOME=/home/vmobj
USER 65532:65532
ENTRYPOINT ["/vmobj"]
