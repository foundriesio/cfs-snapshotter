#!/bin/sh
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Validate release artifacts before anything publishes — runnable locally
# and per-arch in the release workflow.
#
# Usage:
#   release-check.sh binary <arch>       validate bin/containerd-cfs-grpc;
#                                        expects EXPECTED_VERSION and
#                                        EXPECTED_REVISION in the environment
#   release-check.sh archive <tar.gz> <arch>
#   release-check.sh install <tar.gz>    run INSTALL.md's archive steps on
#                                        clean containers (needs docker)
set -eu

fail() { echo "release-check: $*" >&2; exit 1; }

binary() {
    arch=${1:?usage: release-check.sh binary <arch>}
    : "${EXPECTED_VERSION:?}" "${EXPECTED_REVISION:?}"
    bin=bin/containerd-cfs-grpc
    [ -f "$bin" ] || fail "missing $bin"

    case "$arch" in
    amd64) want=x86-64 ;;
    arm64) want=aarch64 ;;
    *) fail "unknown arch: $arch" ;;
    esac
    desc=$(file -b "$bin")
    case "$desc" in
    *ELF*"$want"*) ;;
    *) fail "not an ELF $want binary: $desc" ;;
    esac

    # A static binary makes ldd exit non-zero; the message is the check.
    ldd_out=$(ldd "$bin" 2>&1) || true
    case "$ldd_out" in
    *"not a dynamic executable"* | *"statically linked"*) ;;
    *) fail "not statically linked: $ldd_out" ;;
    esac

    ver_out=$("$bin" -version)
    want_ver="containerd-cfs-grpc ${EXPECTED_VERSION} (${EXPECTED_REVISION})"
    [ "$ver_out" = "$want_ver" ] \
        || fail "-version mismatch: got '$ver_out', want '$want_ver'"
    case "$ver_out" in
    *-dirty*) fail "binary was built from a dirty tree: $ver_out" ;;
    esac

    "$bin" -help 2>&1 | grep -q -- -admit || fail "-help failed"
    echo "release-check: binary OK ($arch, $ver_out)"
}

archive() {
    tarball=${1:?usage: release-check.sh archive <tar.gz> <arch>}
    arch=${2:?usage: release-check.sh archive <tar.gz> <arch>}
    [ -f "$tarball" ] || fail "missing $tarball"
    base=$(basename "$tarball" .tar.gz)

    # Every entry must live under the single top-level directory.
    if tar -tzf "$tarball" | grep -qv "^$base/"; then
        fail "entries outside $base/ in $tarball"
    fi

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    tar -C "$tmp" -xzf "$tarball"
    for f in containerd-cfs-grpc LICENSE README.md INSTALL.md containerd-cfs.toml \
        cfs-snapshotter.service; do
        [ -f "$tmp/$base/$f" ] || fail "missing $f in $tarball"
    done
    count=$(find "$tmp/$base" -type f | wc -l)
    [ "$count" -eq 6 ] || fail "unexpected files in $tarball ($count != 6)"
    [ -x "$tmp/$base/containerd-cfs-grpc" ] \
        || fail "containerd-cfs-grpc is not executable in $tarball"

    "$tmp/$base/containerd-cfs-grpc" -version >/dev/null \
        || fail "archived containerd-cfs-grpc -version failed"
    echo "release-check: archive OK ($tarball)"
}

# The archive install steps exactly as INSTALL.md documents them, on clean
# containers: debian:trixie for the glibc world, alpine for a libc-free
# proof that the binary really is static.
install_test() {
    tarball=${1:?usage: release-check.sh install <tar.gz>}
    [ -f "$tarball" ] || fail "missing $tarball"
    dir=$(realpath "$(dirname "$tarball")")
    name=$(basename "$tarball")

    for image in debian:trixie alpine:3.22; do
        echo "release-check: install test on $image"
        docker run --rm -v "$dir":/rel:ro -e NAME="$name" "$image" sh -s <<'EOF'
set -eux
cd /tmp
tar -xzf "/rel/$NAME"
install -m 0755 "$(basename "$NAME" .tar.gz)/containerd-cfs-grpc" \
    /usr/local/bin/containerd-cfs-grpc
containerd-cfs-grpc -version
containerd-cfs-grpc -help 2>&1 | grep -q -- -admit
EOF
    done
    echo "release-check: install OK ($tarball)"
}

case "${1:?usage: release-check.sh binary|archive|install ...}" in
binary) shift; binary "$@" ;;
archive) shift; archive "$@" ;;
install) shift; install_test "$@" ;;
*) fail "unknown mode: $1" ;;
esac
