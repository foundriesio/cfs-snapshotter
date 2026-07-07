#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CONTAINERD_SOCKET=/run/containerd/containerd.sock
SMOKE_IMAGE=${SMOKE_IMAGE:-127.0.0.1:5000/cfs/smoke:latest}
# The knarr CLI is built on the host and mounted here (the container has no
# credentials for the internal module); used to stage images out of band.
KNARR_BIN=${KNARR_BIN:-/opt/cfs-dev/bin/knarr}

print_versions() {
	echo "runtime versions:"
	docker --version
	dockerd --version
	containerd --version
	ctr --address "${CONTAINERD_SOCKET}" version
	runc --version | sed -n '1,3p'
	zot --version
}

require_cfs_plugin() {
	local plugins
	plugins="$(ctr --address "${CONTAINERD_SOCKET}" plugins ls)"
	if ! grep -q 'io.containerd.snapshotter.v1.*cfs' <<<"${plugins}"; then
		echo "containerd does not list the cfs proxy snapshotter" >&2
		echo "${plugins}" >&2
		exit 1
	fi
}

expect_pull_succeeds() {
	local name="$1"
	local log_file="$2"
	shift 2

	if ! "$@" >"${log_file}" 2>&1; then
		echo "${name} failed; the cfs snapshotter did not complete the pull" >&2
		cat "${log_file}" >&2
		return 1
	fi

	echo "${name} completed through the cfs snapshotter"
}

require_cfs_snapshots() {
	local snaps
	snaps="$(ctr --address "${CONTAINERD_SOCKET}" snapshots ls)"
	if ! grep -qi 'committed' <<<"${snaps}"; then
		echo "cfs recorded no committed snapshots after pull" >&2
		echo "${snaps}" >&2
		return 1
	fi
	echo "cfs recorded committed snapshots"
}

require_stub_commits() {
	if ! grep -q 'committed intermediate virtual snapshot' /var/log/cfs-devcontainer/containerd-cfs-grpc.log; then
		echo "the daemon log records no committed intermediate snapshots" >&2
		return 1
	fi
	echo "handshakes were answered from the knarr-backed index (zero layer fetch)"
}

require_stub_run() {
	local out
	if ! out="$(docker run --rm "${SMOKE_IMAGE}" /hello)"; then
		echo "docker run through the stub rootfs failed" >&2
		return 1
	fi
	if [ "${out}" != "hello from the cfs stub rootfs" ]; then
		echo "docker run output ${out@Q}, want the stub rootfs greeting" >&2
		return 1
	fi
	echo "docker run executed the stand-in rootfs end to end"
}

require_stub_shell_default() {
	local out
	if ! out="$(docker run --rm "${SMOKE_IMAGE}" /bin/sh)"; then
		echo "docker run via a shell default path failed" >&2
		return 1
	fi
	if [ "${out}" != "hello from the cfs stub rootfs" ]; then
		echo "shell-path run output ${out@Q}, want the stub rootfs greeting" >&2
		return 1
	fi
	echo "shell default paths resolve to the greeter"
}

trust_workspace() {
	git config --global --add safe.directory "${ROOT_DIR}" || true
}

require_knarr_cli() {
	[ -x "${KNARR_BIN}" ] ||
		{ echo "knarr CLI not found at ${KNARR_BIN}; devshell.sh builds it on the host" >&2; exit 1; }
}

# stage_out_of_band mirrors what an OTA agent does: the fast-path snapshotter
# never pulls over the network, so the image is staged into the knarr store from
# a local OCI layout before the container runtime pulls it.
stage_out_of_band() {
	local layout="$1" repo digest
	repo="${SMOKE_IMAGE%:*}"
	digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	echo "staging ${repo}@${digest} into the knarr store out of band"
	"${KNARR_BIN}" pull --from-dir "${layout}" "${repo}@${digest}"
}

main() {
	trust_workspace
	print_versions

	make -C "${ROOT_DIR}" fmt
	make -C "${ROOT_DIR}" vet
	make -C "${ROOT_DIR}" test
	make -C "${ROOT_DIR}" build

	ctr --address "${CONTAINERD_SOCKET}" version
	docker version
	docker info
	require_cfs_plugin
	require_knarr_cli

	# Seed the registry and keep the OCI layout, stage it into the knarr store out
	# of band, then let the runtimes pull: the fast-path snapshotter serves the
	# pre-staged chain instead of fetching layers over the network.
	local layout
	layout="$(mktemp -d)/oci"
	SEED_KEEP_LAYOUT="${layout}" "${ROOT_DIR}/.devcontainer/scripts/seed-registry.sh" "${SMOKE_IMAGE}"
	stage_out_of_band "${layout}"

	expect_pull_succeeds "ctr image pull" /tmp/cfs-ctr-pull.log \
		ctr --address "${CONTAINERD_SOCKET}" image pull --plain-http "${SMOKE_IMAGE}"
	require_cfs_snapshots
	expect_pull_succeeds "docker pull" /tmp/cfs-docker-pull.log \
		docker pull "${SMOKE_IMAGE}"
	require_stub_commits
	require_stub_run
	require_stub_shell_default

	echo "cfs served both pulls from the out-of-band knarr store; the container rootfs is still a stand-in — composefs materialization replaces it, after which 'docker run' serves real image content"
}

main "$@"
