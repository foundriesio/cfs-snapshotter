#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

LOG_DIR=/var/log/cfs-devcontainer

stop_pidfile() {
	local name="$1"
	local pidfile="$2"

	if [ ! -s "${pidfile}" ]; then
		return 0
	fi

	local pid
	pid="$(cat "${pidfile}")"
	if kill -0 "${pid}" 2>/dev/null; then
		echo "stopping ${name} (${pid})"
		kill "${pid}"
		for _ in $(seq 1 20); do
			if ! kill -0 "${pid}" 2>/dev/null; then
				break
			fi
			sleep 1
		done
		if kill -0 "${pid}" 2>/dev/null; then
			echo "forcing ${name} (${pid}) to stop"
			kill -KILL "${pid}" 2>/dev/null || true
		fi
	fi
	rm -f "${pidfile}"
}

main() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "clean-runtime-state.sh must run as root" >&2
		exit 1
	fi
	if [ "${1:-}" != "--force" ]; then
		echo "usage: $0 --force" >&2
		echo "refusing to delete nested daemon state without --force" >&2
		exit 1
	fi

	stop_pidfile dockerd "${LOG_DIR}/dockerd.pid"
	stop_pidfile containerd-cfs-grpc "${LOG_DIR}/containerd-cfs-grpc.pid"
	stop_pidfile containerd "${LOG_DIR}/containerd.pid"
	stop_pidfile zot "${LOG_DIR}/zot.pid"
	rm -rf \
		/run/containerd \
		/run/containerd-cfs-grpc \
		/var/lib/containerd \
		/var/lib/containerd-cfs-grpc \
		/var/lib/cfs-dev-registry \
		/var/lib/docker \
		/var/log/cfs-devcontainer
}

main "$@"
