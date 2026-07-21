#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Resolve to the physical path so the workspace label is stable regardless of
# which symlinked path (e.g. a GOPATH symlink) the script is invoked through;
# otherwise `exec` may not match the container that `run` labelled.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
IMAGE="${DEVCONTAINER_IMAGE:-cfs-snapshotter-devcontainer:test}"
DOCKER="${DOCKER:-docker}"
LABEL_DEVSHELL=io.foundries.cfs-snapshotter.devshell
LABEL_WORKSPACE=io.foundries.cfs-snapshotter.workspace

find_running_container() {
	"${DOCKER}" ps -q \
		--filter "label=${LABEL_DEVSHELL}=1" \
		--filter "label=${LABEL_WORKSPACE}=${ROOT_DIR}" |
		sed -n '1p'
}

exec_running_container() {
	local container
	container="$(find_running_container)"
	if [ -z "${container}" ]; then
		echo "no running devshell container found for ${ROOT_DIR}" >&2
		echo "start one with: ./devshell.sh" >&2
		exit 1
	fi

	if [ "$#" -eq 0 ]; then
		set -- bash -il
	fi

	local exec_flags=()
	if [ -t 0 ]; then
		exec_flags+=(-i)
	fi
	if [ -t 1 ]; then
		exec_flags+=(-t)
	fi
	"${DOCKER}" exec "${exec_flags[@]}" -w /workspace "${container}" "$@"
}

# reconcile_cache_ownership hands the shared Go caches back to the host user.
# The devcontainer runs as root (dockerd/containerd need it), so anything the
# container writes into the host-bind-mounted /tmp/cfs-go{cache,modcache}
# lands root-owned; the Go module cache in particular marks its directories
# read-only, so a later host-side `go` invocation can't even delete its way
# out of it, only reset ownership. Run as a trap so it still fires if the
# devshell command fails or is interrupted.
reconcile_cache_ownership() {
	"${DOCKER}" run --rm --entrypoint "" \
		-v /tmp/cfs-gocache:/tmp/cfs-gocache \
		-v /tmp/cfs-gomodcache:/tmp/cfs-gomodcache \
		"${IMAGE}" chown -R "$(id -u):$(id -g)" /tmp/cfs-gocache /tmp/cfs-gomodcache
}

run_new_container() {
	if [ "$#" -eq 0 ]; then
		set -- bash -il
	fi

	mkdir -p /tmp/cfs-gocache /tmp/cfs-gomodcache

	"${DOCKER}" build -f "${ROOT_DIR}/.devcontainer/Dockerfile" -t "${IMAGE}" "${ROOT_DIR}/.devcontainer"
	trap reconcile_cache_ownership EXIT

	local run_flags=(--rm --privileged)
	if [ -t 0 ]; then
		run_flags+=(-i)
	fi
	if [ -t 1 ]; then
		run_flags+=(-t)
	fi

	# The container has no credentials for the internal knarr module, so build the
	# knarr CLI on the host (which does) at the version cfs pins and mount the
	# binary in, so the container has a knarr CLI matching the pinned module.
	# Portable across setups and CI, unlike bind-mounting a checkout. Best-effort:
	# a plain shell still opens when the CLI is not needed or cannot be built.
	local knarr_version
	if knarr_version="$(cd "${ROOT_DIR}" && go list -m -f '{{.Version}}' github.com/foundriesio/knarr 2>/dev/null)" &&
		GOBIN=/tmp/cfs-knarr-bin GOTOOLCHAIN=auto GOPRIVATE=github.com/foundriesio go install "github.com/foundriesio/knarr/cmd/knarr@${knarr_version}"; then
		run_flags+=(-v /tmp/cfs-knarr-bin/knarr:/opt/cfs-dev/bin/knarr:ro)
	else
		echo "warning: could not build the knarr CLI on the host; any e2e that needs it will fail" >&2
	fi

	"${DOCKER}" run "${run_flags[@]}" \
		--label "${LABEL_DEVSHELL}=1" \
		--label "${LABEL_WORKSPACE}=${ROOT_DIR}" \
		--security-opt=seccomp=unconfined \
		--security-opt=apparmor=unconfined \
		--cgroupns=host \
		-e CFS_DEVCONTAINER=1 \
		-e PATH=/opt/cfs-dev/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		-e GOCACHE=/tmp/cfs-gocache \
		-e GOMODCACHE=/tmp/cfs-gomodcache \
		-v /tmp/cfs-gocache:/tmp/cfs-gocache \
		-v /tmp/cfs-gomodcache:/tmp/cfs-gomodcache \
		-v "${ROOT_DIR}:/workspace" \
		-w /workspace \
		"${IMAGE}" \
		"$@"
}

if [ "${1:-}" = "exec" ]; then
	shift
	exec_running_container "$@"
else
	run_new_container "$@"
fi
