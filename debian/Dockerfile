# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Packaging image for the cfs-snapshotter .deb (Makefile deb-image/deb
# targets). Debian trixie's golang (1.24) cannot build the module (go.mod
# requires 1.26), so the toolchain comes from the official golang image.
# Keep GO_VERSION in sync with .devcontainer/Dockerfile.
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-trixie AS go

FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        debhelper \
        devscripts \
        fakeroot \
        lintian \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
WORKDIR /build
