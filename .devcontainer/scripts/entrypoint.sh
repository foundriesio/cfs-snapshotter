#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

ROOT_DIR=/workspace
LOG_DIR=/var/log/cfs-devcontainer
CONTAINERD_CFS_GRPC_BIN="${ROOT_DIR}/bin/containerd-cfs-grpc"
CFS_SOCKET=/run/containerd-cfs-grpc/containerd-cfs-grpc.sock
CONTAINERD_SOCKET=/run/containerd/containerd.sock
CFS_LOG_LEVEL="${CFS_LOG_LEVEL:-trace}"
CFS_STUB_ROOTFS="${CFS_STUB_ROOTFS:-/tmp/cfs-stub-rootfs}"
CFS_ROOT=/var/lib/containerd-cfs-grpc
CFS_ROOT_IMG=/var/tmp/cfs-root.img
EROFS_ROOT=/var/lib/containerd/io.containerd.snapshotter.v1.erofs
EROFS_ROOT_IMG=/var/tmp/erofs-root.img
KNARR_ROOT=/var/lib/knarr
KNARR_VERITY_IMG=/var/tmp/knarr-verity.img
STORE_SIZE_MB="${STORE_SIZE_MB:-512}"
CFS_STATE_FILE="${LOG_DIR}/containerd-cfs-grpc.env"
REGISTRY_URL=http://127.0.0.1:5000


wait_for() {
	local name="$1"
	shift

	for _ in $(seq 1 60); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done

	echo "timed out waiting for ${name}" >&2
	return 1
}

start_daemon() {
	local name="$1"
	local pidfile="$2"
	local logfile="$3"
	shift 3

	if [ -s "${pidfile}" ] && kill -0 "$(cat "${pidfile}")" 2>/dev/null; then
		echo "${name} already running with pid $(cat "${pidfile}")"
		return 0
	fi

	echo "starting ${name}"
	nohup "$@" >"${logfile}" 2>&1 &
	echo "$!" >"${pidfile}"
}

# mount_loop_ext4 mounts a fresh loop-backed ext4 filesystem at dir, enabling the
# fs-verity feature when the third argument is "verity" (which requires a
# 4096-byte block size). A filesystem already mounted at dir is left as is.
mount_loop_ext4() {
	local img="$1" dir="$2" verity="${3:-}"
	if mountpoint -q "${dir}" 2>/dev/null; then
		return 0
	fi
	local stale dev
	stale="$(losetup -a 2>/dev/null | grep -F "${img}" | cut -d: -f1 || true)"
	for dev in ${stale}; do
		losetup -d "${dev}" 2>/dev/null || true
	done
	mkdir -p "${dir}"
	dd if=/dev/zero of="${img}" bs=1M count="${STORE_SIZE_MB}" status=none
	if [ -n "${verity}" ]; then
		mkfs.ext4 -q -F -b 4096 -O verity "${img}"
	else
		mkfs.ext4 -q -F "${img}"
	fi
	dev="$(losetup --find --show "${img}")"
	mount "${dev}" "${dir}"
}

# setup_real_filesystems backs the knarr store and the snapshotter root with real
# filesystems. fs-verity needs a real backing filesystem the container overlay
# cannot provide, and the snapshotter's writable composefs overlay upper cannot
# live on the container's own overlayfs, since overlayfs cannot stack on
# overlayfs.
setup_real_filesystems() {
	mount_loop_ext4 "${KNARR_VERITY_IMG}" "${KNARR_ROOT}" verity
	mount_loop_ext4 "${CFS_ROOT_IMG}" "${CFS_ROOT}"
	echo "knarr store at ${KNARR_ROOT} (ext4+verity); snapshotter root at ${CFS_ROOT} (ext4)"
}

start_runtime_daemons() {
	mkdir -p \
		/run/containerd \
		/run/containerd-cfs-grpc \
		/var/lib/containerd \
		/var/lib/containerd-cfs-grpc \
		/var/lib/cfs-dev-registry \
		/var/lib/docker \
		"${LOG_DIR}"

	echo "runtime versions:"
	docker --version
	dockerd --version
	containerd --version
	ctr --help | sed -n '1p'
	runc --version | sed -n '1,3p'
	zot --version

	if [ -d "${ROOT_DIR}/.git" ]; then
		git config --global --add safe.directory "${ROOT_DIR}" || true
	fi

	start_daemon zot "${LOG_DIR}/zot.pid" "${LOG_DIR}/zot.log" \
		zot serve /etc/zot/config.json
	wait_for zot curl -fsS "${REGISTRY_URL}/v2/"

	make -C "${ROOT_DIR}" build
	setup_real_filesystems
	# The snapshotter requires a stand-in rootfs; build the default one when
	# the caller does not supply a directory.
	if [ ! -e "${CFS_STUB_ROOTFS}/hello" ] || [ ! -e "${CFS_STUB_ROOTFS}/bin/sh" ]; then
		"${ROOT_DIR}/.devcontainer/scripts/make-stub-rootfs.sh" "${CFS_STUB_ROOTFS}"
	fi
	rm -f "${CFS_SOCKET}"
	start_daemon containerd-cfs-grpc "${LOG_DIR}/containerd-cfs-grpc.pid" "${LOG_DIR}/containerd-cfs-grpc.log" \
		"${CONTAINERD_CFS_GRPC_BIN}" -log-level "${CFS_LOG_LEVEL}" \
		-stub-rootfs "${CFS_STUB_ROOTFS}" -knarr-root "${KNARR_ROOT}"
	printf '%s\n' "${CFS_LOG_LEVEL} ${CFS_STUB_ROOTFS} ${KNARR_ROOT}" >"${CFS_STATE_FILE}"
	wait_for cfs-snapshotter test -S "${CFS_SOCKET}"

	# Enable the built-in erofs snapshotter only when the kernel supports
	# EROFS: its unpack_config entry is resolved by the transfer service at
	# containerd startup, so on a kernel without EROFS the entry would fail
	# the whole transfer plugin and break every pull. Its overlay uppers also
	# need a real filesystem, like the cfs root.
	if grep -qw erofs /proc/filesystems; then
		if ! grep -q 'snapshotter = "erofs"' /etc/containerd/config.toml; then
			cat /etc/containerd/containerd-erofs-unpack.toml >>/etc/containerd/config.toml
		fi
		mount_loop_ext4 "${EROFS_ROOT_IMG}" "${EROFS_ROOT}"
	else
		echo "host kernel lacks EROFS; erofs snapshotter comparison disabled"
	fi
	start_daemon containerd "${LOG_DIR}/containerd.pid" "${LOG_DIR}/containerd.log" \
		containerd --config /etc/containerd/config.toml
	wait_for containerd ctr --address "${CONTAINERD_SOCKET}" version
	ctr --address "${CONTAINERD_SOCKET}" version

	start_daemon dockerd "${LOG_DIR}/dockerd.pid" "${LOG_DIR}/dockerd.log" \
		dockerd --host=unix:///var/run/docker.sock --containerd="${CONTAINERD_SOCKET}"
	wait_for dockerd docker version
	wait_for docker-info docker info

	echo "devcontainer daemons are ready"
	echo "logs: ${LOG_DIR}"
}

main() {
	if [ "$#" -eq 0 ]; then
		set -- sleep infinity
	fi

	start_runtime_daemons

	exec "$@"
}

main "$@"
