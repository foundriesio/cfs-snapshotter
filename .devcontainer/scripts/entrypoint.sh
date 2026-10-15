#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer
. "${SCRIPT_DIR}/shared-root.sh"

ROOT_DIR=/workspace
LOG_DIR=/var/log/cfs-devcontainer
CONTAINERD_CFS_GRPC_BIN="${ROOT_DIR}/bin/containerd-cfs-grpc"
CFS_SOCKET=/run/containerd-cfs-grpc/containerd-cfs-grpc.sock
CONTAINERD_SOCKET=/run/containerd/containerd.sock
CFS_LOG_LEVEL="${CFS_LOG_LEVEL:-trace}"
CFS_ROOT=/var/lib/containerd-cfs-grpc
EROFS_ROOT=/var/lib/containerd/io.containerd.snapshotter.v1.erofs
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
	# setsid puts each daemon in its own session so a SIGINT to the container's
	# foreground process group (an interrupted docker run, a Ctrl-C in the
	# devshell) does not take them all down together; nohup keeps SIGHUP/stdio.
	setsid nohup "$@" >"${logfile}" 2>&1 &
	echo "$!" >"${pidfile}"
}

start_runtime_daemons() {
	mkdir -p \
		/run/containerd \
		/run/containerd-cfs-grpc \
		/var/lib/containerd \
		/var/lib/containerd-cfs-grpc \
		/var/lib/cfs-dev-registry \
		/var/lib/docker \
		/var/lib/ocicfs \
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
	# One loop-backed ext4 with fs-verity hosts the ocicfs store and every
	# snapshotter root; loop nodes are scarce in the container's tmpfs /dev.
	mount_loop_ext4 "${SHARED_ROOT_IMG}" "${SHARED_ROOT}"
	bind_root "${CFS_ROOT}"
	bind_root "${OCICFS_ROOT}"
	rm -f "${CFS_SOCKET}"
	start_daemon containerd-cfs-grpc "${LOG_DIR}/containerd-cfs-grpc.pid" "${LOG_DIR}/containerd-cfs-grpc.log" \
		"${CONTAINERD_CFS_GRPC_BIN}" -log-level "${CFS_LOG_LEVEL}" -root "${CFS_ROOT}" -ocicfs-root "${OCICFS_ROOT}" \
		-allow-unstaged-pulls -ingest-workers 0
	# The state line run-cfs.sh reconciles against: log level, stores, work
	# mode, ingest workers (0 is the daemon's default).
	printf '%s\n' "${CFS_LOG_LEVEL} ${OCICFS_ROOT} ${CFS_ROOT} 1 0" >"${CFS_STATE_FILE}"
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
		bind_root "${EROFS_ROOT}"
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
