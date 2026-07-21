# SPDX-License-Identifier: Apache-2.0

ARG GO_VERSION=1.26
FROM mcr.microsoft.com/devcontainers/go:${GO_VERSION}-bookworm

ARG TARGETARCH
ARG TARGETVARIANT
ARG DOCKER_VERSION=29.3.0
ARG CONTAINERD_VERSION=2.2.2
ARG RUNC_VERSION=1.3.3
ARG CNI_PLUGINS_VERSION=1.8.0
ARG ZOT_VERSION=2.1.17
ARG EROFS_UTILS_VERSION=1.9.2
ARG COMPOSEFS_VERSION=v1.0.8
ARG COMPOSEFS_COMMIT=858ce1b38e1534c2602eb431124b5dca706bc746
ARG GOLANGCI_LINT_VERSION=2.12.2

ENV CFS_DEVCONTAINER=1
ENV PATH="/opt/cfs-dev/bin:${PATH}"
ENV CONTAINERD_SNAPSHOTTER=cfs

RUN set -eux; \
    install -d /opt/cfs-devcontainer; \
    printf 'cfs-snapshotter devcontainer\n' >/opt/cfs-devcontainer/.runtime-marker; \
    printf 'export PATH="/opt/cfs-dev/bin:${PATH}"\n' >/etc/profile.d/cfs-devcontainer-path.sh; \
    printf '%s\n' \
        'CFS_SNAPSHOTTER_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log' \
        'cfs-log() { jq . "${CFS_SNAPSHOTTER_LOG}"; }' \
        'cfs-log-follow() { tail -F "${CFS_SNAPSHOTTER_LOG}" | jq .; }' \
        >/etc/profile.d/cfs-devcontainer-log.sh; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        autoconf \
        automake \
        ca-certificates \
        curl \
        dpkg-dev \
        e2fsprogs \
        fuse3 \
        git \
        iproute2 \
        iptables \
        jq \
        kmod \
        libssl-dev \
        libtool \
        make \
        meson \
        ninja-build \
        pkg-config \
        procps \
        util-linux \
        uuid-dev; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
        amd64) docker_arch="x86_64"; containerd_arch="amd64"; runc_arch="amd64"; cni_arch="amd64"; zot_arch="amd64"; golangci_lint_arch="amd64" ;; \
        arm64) docker_arch="aarch64"; containerd_arch="arm64"; runc_arch="arm64"; cni_arch="arm64"; zot_arch="arm64"; golangci_lint_arch="arm64" ;; \
        armv7) docker_arch="armhf"; containerd_arch="arm-v7"; runc_arch="armhf"; cni_arch="arm"; zot_arch="arm"; golangci_lint_arch="armv7" ;; \
        *) echo "unsupported target architecture: ${TARGETARCH}${TARGETVARIANT}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/cfs-dev/bin /tmp/cfs-install; \
    cd /tmp/cfs-install; \
    curl -fL --retry 3 --output docker.tgz \
        "https://download.docker.com/linux/static/stable/${docker_arch}/docker-${DOCKER_VERSION}.tgz"; \
    tar -xzf docker.tgz; \
    install -m 0755 docker/docker docker/dockerd docker/docker-init docker/docker-proxy /opt/cfs-dev/bin/; \
    curl -fL --retry 3 --output containerd.tgz \
        "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${containerd_arch}.tar.gz"; \
    tar -xzf containerd.tgz; \
    install -m 0755 bin/containerd bin/containerd-shim-runc-v2 bin/ctr /opt/cfs-dev/bin/; \
    curl -fL --retry 3 --output runc \
        "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${runc_arch}"; \
    install -m 0755 runc /opt/cfs-dev/bin/runc; \
    curl -fL --retry 3 --output cni-plugins.tgz \
        "https://github.com/containernetworking/plugins/releases/download/v${CNI_PLUGINS_VERSION}/cni-plugins-linux-${cni_arch}-v${CNI_PLUGINS_VERSION}.tgz"; \
    mkdir -p /opt/cni/bin; \
    tar -xzf cni-plugins.tgz -C /opt/cni/bin; \
    curl -fL --retry 3 --output zot \
        "https://github.com/project-zot/zot/releases/download/v${ZOT_VERSION}/zot-linux-${zot_arch}"; \
    install -m 0755 zot /opt/cfs-dev/bin/zot; \
    # Debian's erofs-utils predates tar mode, which containerd's erofs differ
    # requires, so build the pinned release from source.
    curl -fL --retry 3 --output erofs-utils.tgz \
        "https://github.com/erofs/erofs-utils/archive/refs/tags/v${EROFS_UTILS_VERSION}.tar.gz"; \
    tar -xzf erofs-utils.tgz; \
    cd "erofs-utils-${EROFS_UTILS_VERSION}"; \
    ./autogen.sh; \
    ./configure; \
    make -j"$(nproc)"; \
    install -m 0755 mkfs/mkfs.erofs dump/dump.erofs fsck/fsck.erofs /opt/cfs-dev/bin/; \
    mkfs.erofs --help 2>&1 | grep -q -- --tar; \
    cd ..; \
    # knarr shells out to mkcomposefs and mount.composefs; build the pinned
    # composefs release from source (Debian ships no composefs tools).
    MULTIARCH="$(dpkg-architecture -qDEB_HOST_MULTIARCH)"; \
    git clone --depth 1 --branch "${COMPOSEFS_VERSION}" https://github.com/containers/composefs.git /tmp/composefs; \
    test "$(git -C /tmp/composefs rev-parse HEAD)" = "${COMPOSEFS_COMMIT}"; \
    meson setup /tmp/composefs/build /tmp/composefs --prefix=/usr --libdir="/usr/lib/${MULTIARCH}" -Dfuse=disabled -Dman=disabled; \
    meson compile -C /tmp/composefs/build; \
    meson install -C /tmp/composefs/build; \
    curl -fL --retry 3 --output golangci-lint.tgz \
        "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${golangci_lint_arch}.tar.gz"; \
    tar -xzf golangci-lint.tgz; \
    install -m 0755 "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${golangci_lint_arch}/golangci-lint" /opt/cfs-dev/bin/; \
    docker --version; \
    containerd --version; \
    ctr --help >/dev/null; \
    runc --version; \
    zot --version; \
    mkfs.erofs --version; \
    golangci-lint --version; \
    test -x /opt/cni/bin/loopback; \
    rm -rf /tmp/cfs-install /tmp/composefs

COPY config/ /tmp/cfs-devcontainer-config/

RUN set -eux; \
    install -d /etc/containerd/certs.d /etc/docker /etc/zot; \
    cp -a /tmp/cfs-devcontainer-config/containerd-certs.d/. /etc/containerd/certs.d/; \
    install -m 0644 /tmp/cfs-devcontainer-config/containerd-config.toml /etc/containerd/config.toml; \
    install -m 0644 /tmp/cfs-devcontainer-config/containerd-erofs-unpack.toml /etc/containerd/containerd-erofs-unpack.toml; \
    install -m 0644 /tmp/cfs-devcontainer-config/docker-daemon.json /etc/docker/daemon.json; \
    install -m 0644 /tmp/cfs-devcontainer-config/zot-config.json /etc/zot/config.json; \
    rm -rf /tmp/cfs-devcontainer-config

ENTRYPOINT ["/workspace/.devcontainer/scripts/entrypoint.sh"]
# Keep editor-managed devcontainer sessions alive when no command is provided.
CMD ["sleep", "infinity"]
