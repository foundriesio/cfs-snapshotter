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

# The host directories behind the container's Go caches and the knarr CLI it
# is handed. They live under the user's cache directory rather than /tmp: a
# tmpfs /tmp with a user quota fills up with a root-written build cache, and
# then the ownership hand-back below fails and so does every other write.
CACHE_DIR="${CFS_DEV_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/cfs-snapshotter}"
GOCACHE_DIR="${CACHE_DIR}/go-build"
GOMODCACHE_DIR="${CACHE_DIR}/go-mod"
KNARR_BIN_DIR="${CACHE_DIR}/bin"

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
# container writes into the host-bind-mounted Go caches lands root-owned; the Go module cache in particular marks its directories
# read-only, so a later host-side `go` invocation can't even delete its way
# out of it, only reset ownership. Run as a trap so it still fires if the
# devshell command fails or is interrupted.
reconcile_cache_ownership() {
	"${DOCKER}" run --rm --entrypoint "" \
		-v "${GOCACHE_DIR}:/tmp/cfs-gocache" \
		-v "${GOMODCACHE_DIR}:/tmp/cfs-gomodcache" \
		"${IMAGE}" chown -R "$(id -u):$(id -g)" /tmp/cfs-gocache /tmp/cfs-gomodcache
}

run_new_container() {
	if [ "$#" -eq 0 ]; then
		set -- bash -il
	fi

	mkdir -p "${GOCACHE_DIR}" "${GOMODCACHE_DIR}" "${KNARR_BIN_DIR}"

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
	#
	# This also populates the shared module cache with the pinned knarr module, so
	# the container's own `go build` (same GOMODCACHE, bind-mounted below) finds
	# it without needing network access.
	# Both go commands run against the shared module cache rather than the
	# caller's: the version probe needs the pinned module's go.mod, and a
	# caller cache that misses it would send the probe to the network — where
	# a not-yet-pushed pin does not resolve. GOFLAGS is cleared because flags
	# like -mod are rejected by `go install pkg@version`, and a stray export
	# would fail the build.
	local knarr_goenv=(GOMODCACHE="${GOMODCACHE_DIR}" GOTOOLCHAIN=auto GOPRIVATE=github.com/foundriesio GOFLAGS=)
	local knarr_version
	if knarr_version="$(cd "${ROOT_DIR}" && env "${knarr_goenv[@]}" go list -m -f '{{.Version}}' github.com/foundriesio/knarr)" &&
		env "${knarr_goenv[@]}" GOBIN="${KNARR_BIN_DIR}" go install "github.com/foundriesio/knarr/cmd/knarr@${knarr_version}"; then
		run_flags+=(-v "${KNARR_BIN_DIR}/knarr:/opt/cfs-dev/bin/knarr:ro")
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
		-e CFS_SHARED_ROOT_SIZE_MB \
		-e PATH=/opt/cfs-dev/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		-e GOCACHE=/tmp/cfs-gocache \
		-e GOMODCACHE=/tmp/cfs-gomodcache \
		-v "${GOCACHE_DIR}:/tmp/cfs-gocache" \
		-v "${GOMODCACHE_DIR}:/tmp/cfs-gomodcache" \
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
