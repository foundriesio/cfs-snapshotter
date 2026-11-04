#!/bin/sh
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Install-test the built .deb on clean Debian and Ubuntu containers
# (Makefile deb-test target). For each distro: assert the control fields,
# install, check the stamped version and the shipped unit and env files,
# reinstall (the upgrade path), then remove and purge. Plain containers have
# no systemd, so the unit is checked as an installed file, not started.
# Expects DEB_VERSION and VERSION in the environment and the .deb directory
# as the only argument.
set -eu

out_dir=${1:?usage: deb-test.sh <deb-dir>}
: "${DEB_VERSION:?}" "${VERSION:?}"

arch=$(dpkg --print-architecture 2>/dev/null \
    || uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
deb="$out_dir/cfs-snapshotter_${DEB_VERSION}_${arch}.deb"
[ -f "$deb" ] || { echo "deb-test: missing $deb" >&2; exit 1; }

for image in debian:trixie ubuntu:noble; do
    echo "deb-test: $image"
    docker run --rm \
        -v "$(realpath "$out_dir")":/out:ro \
        -e DEB_VERSION -e VERSION -e ARCH="$arch" \
        "$image" bash -s <<'EOF'
set -eux
deb="/out/cfs-snapshotter_${DEB_VERSION}_${ARCH}.deb"

[ "$(dpkg-deb -f "$deb" Package)" = cfs-snapshotter ]
[ "$(dpkg-deb -f "$deb" Version)" = "$DEB_VERSION" ]
[ "$(dpkg-deb -f "$deb" Architecture)" = "$ARCH" ]
dpkg-deb -f "$deb" Recommends | grep -q containerd
dpkg-deb -f "$deb" Recommends | grep -q composefs
dpkg-deb -f "$deb" Suggests | grep -q ocicfs

apt-get update
apt-get install -y --no-install-recommends "$deb"
containerd-cfs-grpc -version | grep -F "containerd-cfs-grpc ${VERSION}"
containerd-cfs-grpc -help 2>&1 | grep -q -- -admit
[ -f /usr/lib/systemd/system/cfs-snapshotter.service ] \
    || [ -f /lib/systemd/system/cfs-snapshotter.service ]
grep -q RestartPreventExitStatus=3 /usr/lib/systemd/system/cfs-snapshotter.service
containerd-cfs-grpc -help 2>&1 | grep -q -- -check
grep -q CFS_OPTS /etc/default/cfs-snapshotter
grep -q proxy_plugins /usr/share/doc/cfs-snapshotter/containerd-cfs.toml

# Installing the same package again exercises the upgrade path.
apt-get install -y --reinstall --no-install-recommends "$deb"
containerd-cfs-grpc -version >/dev/null

apt-get remove -y cfs-snapshotter
! command -v containerd-cfs-grpc
# The env file is a conffile: remove keeps it, purge drops it.
grep -q CFS_OPTS /etc/default/cfs-snapshotter
apt-get purge -y cfs-snapshotter
[ ! -e /etc/default/cfs-snapshotter ]
EOF
done
echo "deb-test: OK"
