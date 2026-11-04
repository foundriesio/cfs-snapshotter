#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer
. "${SCRIPT_DIR}/shared-root.sh"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOG_DIR=/var/log/cfs-devcontainer
CONTAINERD_CFS_GRPC_BIN="${ROOT_DIR}/bin/containerd-cfs-grpc"
CFS_SOCKET=/run/containerd-cfs-grpc/containerd-cfs-grpc.sock
CONTAINERD_SOCKET=/run/containerd/containerd.sock
CFS_LOG_LEVEL="${CFS_LOG_LEVEL:-trace}"
CFS_ROOT="${CFS_ROOT:-/var/lib/containerd-cfs-grpc}"
# any or preloaded, passed to the daemon as -admit.
CFS_ADMIT="${CFS_ADMIT:-any}"
# 0 selects the daemon's default worker count.
CFS_INGEST_WORKERS="${CFS_INGEST_WORKERS:-0}"
CFS_PIDFILE="${LOG_DIR}/containerd-cfs-grpc.pid"
CFS_LOGFILE="${LOG_DIR}/containerd-cfs-grpc.log"
CFS_STATE_FILE="${LOG_DIR}/containerd-cfs-grpc.env"

require_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "run-cfs.sh must run as root" >&2
		exit 1
	fi
}

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

pidfile_running() {
	local pidfile="$1"

	[ -s "${pidfile}" ] && kill -0 "$(cat "${pidfile}")" 2>/dev/null
}

restart_daemon() {
	local name="$1"
	local pidfile="$2"
	local socket="$3"
	local logfile="$4"
	shift 4

	stop_pidfile "${name}" "${pidfile}"
	rm -f "${socket}"
	start_daemon "${name}" "${pidfile}" "${logfile}" "$@"
}

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

file_hash() {
	local file="$1"

	if [ ! -f "${file}" ]; then
		return 0
	fi
	sha256sum "${file}" | awk '{print $1}'
}

cfs_plugin_ready() {
	local plugins
	plugins="$(ctr --address "${CONTAINERD_SOCKET}" plugins ls 2>/dev/null || true)"
	grep -q 'io.containerd.snapshotter.v1.*cfs.*ok' <<<"${plugins}"
}

restart_containerd_for_proxy_registration() {
	stop_pidfile containerd "${LOG_DIR}/containerd.pid"
	rm -f "${CONTAINERD_SOCKET}" "${CONTAINERD_SOCKET}.ttrpc"

	start_daemon containerd "${LOG_DIR}/containerd.pid" "${LOG_DIR}/containerd.log" \
		containerd --config /etc/containerd/config.toml
	wait_for containerd ctr --address "${CONTAINERD_SOCKET}" version
}

main() {
	require_root

	mkdir -p \
		/run/containerd \
		/run/containerd-cfs-grpc \
		/var/lib/containerd \
		/var/lib/docker \
		"${CFS_ROOT}" \
		"${OCICFS_ROOT}" \
		"${LOG_DIR}"

	git config --global --add safe.directory "${ROOT_DIR}" || true
	mount_loop_ext4 "${SHARED_ROOT_IMG}" "${SHARED_ROOT}"
	bind_root "${CFS_ROOT}"
	bind_root "${OCICFS_ROOT}"
	local old_hash new_hash old_state new_state
	# The daemon's work mode, its stores and its worker count are part of the
	# state a restart reconciles: changing any of them makes the running
	# daemon the wrong one.
	new_state="${CFS_LOG_LEVEL} ${OCICFS_ROOT} ${CFS_ROOT} ${CFS_ADMIT} ${CFS_INGEST_WORKERS}"
	old_hash="$(file_hash "${CONTAINERD_CFS_GRPC_BIN}")"
	old_state="$(cat "${CFS_STATE_FILE}" 2>/dev/null || true)"
	make -C "${ROOT_DIR}" build
	new_hash="$(file_hash "${CONTAINERD_CFS_GRPC_BIN}")"

	if pidfile_running "${CFS_PIDFILE}" &&
		[ -S "${CFS_SOCKET}" ] &&
		[ "${CFS_FORCE_RESTART:-0}" != "1" ] &&
		[ "${old_hash}" = "${new_hash}" ] &&
		[ "${old_state}" = "${new_state}" ]; then
		echo "cfs snapshotter binary unchanged; keeping pid $(cat "${CFS_PIDFILE}")"
	else
		# Always explicit: the mode is part of the reconciled state, so the
		# daemon must be started with the value the state line records.
		local args=(-log-level "${CFS_LOG_LEVEL}" -root "${CFS_ROOT}" -ocicfs-root "${OCICFS_ROOT}"
			"-admit=${CFS_ADMIT}" -ingest-workers "${CFS_INGEST_WORKERS}")
		restart_daemon containerd-cfs-grpc \
			"${CFS_PIDFILE}" \
			"${CFS_SOCKET}" \
			"${CFS_LOGFILE}" \
			"${CONTAINERD_CFS_GRPC_BIN}" "${args[@]}"
		printf '%s\n' "${new_state}" >"${CFS_STATE_FILE}"
	fi
	wait_for cfs-snapshotter test -S "${CFS_SOCKET}"

	if ! cfs_plugin_ready; then
		echo "cfs proxy plugin is not registered; restarting containerd"
		restart_containerd_for_proxy_registration
	fi
	wait_for cfs-proxy-plugin cfs_plugin_ready

	echo "cfs snapshotter is running"
	echo "log level: ${CFS_LOG_LEVEL}"
	echo "roots: ${CFS_ROOT} ${OCICFS_ROOT}"
	echo "admit: ${CFS_ADMIT}"
	echo "ingest workers: ${CFS_INGEST_WORKERS}"
	echo "logs: ${CFS_LOGFILE}"
}

main "$@"
