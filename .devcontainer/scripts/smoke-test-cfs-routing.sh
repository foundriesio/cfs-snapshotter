#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CONTAINERD_SOCKET=/run/containerd/containerd.sock
SMOKE_IMAGE=${SMOKE_IMAGE:-127.0.0.1:5000/cfs/smoke:latest}

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

expect_not_implemented() {
	local name="$1"
	local log_file="$2"
	shift 2

	if "$@" >"${log_file}" 2>&1; then
		echo "${name} unexpectedly succeeded; update this smoke test for the implemented backend" >&2
		return 1
	fi

	if ! grep -qi 'not implemented' "${log_file}"; then
		echo "${name} failed before reaching the cfs stub" >&2
		cat "${log_file}" >&2
		return 1
	fi

	echo "${name} reached cfs and failed with not implemented as expected"
}

trust_workspace() {
	git config --global --add safe.directory "${ROOT_DIR}" || true
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
	"${ROOT_DIR}/.devcontainer/scripts/seed-registry.sh" "${SMOKE_IMAGE}"

	expect_not_implemented "ctr image pull" /tmp/cfs-ctr-pull.log \
		ctr --address "${CONTAINERD_SOCKET}" image pull --plain-http --snapshotter cfs "${SMOKE_IMAGE}"
	expect_not_implemented "docker pull" /tmp/cfs-docker-pull.log \
		docker pull "${SMOKE_IMAGE}"

	echo "future backend check: pulls succeed and 'docker run --rm ${SMOKE_IMAGE} true' works once the backend is implemented"
}

main "$@"
