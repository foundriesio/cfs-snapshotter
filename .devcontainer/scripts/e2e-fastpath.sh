#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

# Drives the full fast path end to end: pull a multi-layer image into the knarr
# store out of band, create and run containers from it through docker, and
# assert that the snapshotter served them from a content-addressed composefs
# mount without fetching any layer, that a container executes and prints from
# that rootfs, that writes land in the overlay while the image stays immutable,
# that a container's writes persist across its stop/start while a fresh
# container starts clean, that stop/rm/rmi tear the container and then the
# image mount down, that a real-world image runs digest-pinned from its home
# registry with only manifest and config fetched, that an unmaterialized image
# fails closed, that a tampered store object is rejected by fs-verity, and
# that the snapshotter recovers after a restart.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

CONTAINERD_SOCKET=/run/containerd/containerd.sock
REGISTRY=127.0.0.1:5000
DOCKER_NS=moby
LAYER_COUNT=3
KNARR_ROOT=/var/lib/knarr
KNARR_BIN=/opt/cfs-dev/bin/knarr
KNARR_SRC=/work/go/src/github.com/foundriesio/knarr
CFS_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log
# Real-world image for the registry round-trip check; needs network access to
# its home registry (manifest and config only — layers must not be fetched).
REAL_IMAGE="${CFS_E2E_REAL_IMAGE:-docker.io/library/busybox:latest}"

work=""

cleanup() {
	docker rm -f e2e-run e2e-persist e2e-daemon e2e-restart >/dev/null 2>&1 || true
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
}

fail() {
	echo "E2E FAIL: $1" >&2
	echo "---- cfs snapshotter log ----" >&2
	tail -80 "${CFS_LOG}" >&2 2>/dev/null || true
	echo "---- containerd log ----" >&2
	tail -60 /var/log/cfs-devcontainer/containerd.log >&2 2>/dev/null || true
	echo "---- moby content ----" >&2
	ctr --address "${CONTAINERD_SOCKET}" -n "${DOCKER_NS}" content ls >&2 2>/dev/null || true
	echo "---- cfs snapshots ----" >&2
	ctr --address "${CONTAINERD_SOCKET}" -n "${DOCKER_NS}" snapshots --snapshotter cfs ls >&2 2>/dev/null || true
	exit 1
}

# content_has reports whether a digest is in containerd's content store. A
# failing ctr aborts the test: treating it as "absent" would turn a broken
# content service into a false pass of the zero-fetch assertions.
content_has() {
	local listing
	listing="$(ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NS}" content ls -q)" ||
		fail "ctr content ls failed"
	grep -Fxq "$1" <<<"${listing}"
}

manifest_body() {
	curl -fsS -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
		"http://${REGISTRY}/v2/$1/manifests/$2"
}

manifest_digest() {
	curl -fsSI -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
		"http://${REGISTRY}/v2/$1/manifests/$2" |
		awk 'tolower($1) == "docker-content-digest:" {gsub(/\r/, "", $2); print $2}'
}

build_knarr() {
	echo "building knarr CLI from ${KNARR_SRC}"
	# -buildvcs=false: the bind-mounted source's .git is owned by the host user,
	# which git inside the container refuses to read.
	(cd "${KNARR_SRC}" && GOTOOLCHAIN=auto go build -buildvcs=false -o "${KNARR_BIN}" ./cmd/knarr)
}

# chain_mount prints the newest shared image metadata mount: the snapshotter mounts
# each image once, at its chain snapshot's fs directory, and containers overlay
# on top of it.
chain_mount() {
	awk '$2 ~ /containerd-cfs-grpc\/snapshots\/[0-9]+\/fs$/ {print $2}' /proc/self/mounts | tail -1
}

# seed_and_pull seeds a fresh multi-layer image into the registry, keeps its OCI
# layout, and pulls it into the knarr store by digest. Prints the image ref.
seed_and_pull() {
	local nonce="$1" repo="$2" ref layout digest
	ref="${REGISTRY}/${repo}:${nonce}"
	layout="${work}/${repo//\//_}-${nonce}"
	SEED_LAYER_COUNT="${LAYER_COUNT}" SEED_NONCE="${nonce}" SEED_KEEP_LAYOUT="${layout}" \
		"${SCRIPT_DIR}/seed-registry.sh" "${ref}" >&2
	digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	"${KNARR_BIN}" pull --from-dir "${layout}" "${repo}@${digest}" >&2 ||
		fail "knarr pull of ${repo}@${digest} failed"
	echo "${ref}"
}

assert_fastpath_run() {
	local nonce="1${RANDOM}${RANDOM}"
	local repo="cfs/e2e" ref
	ref="$(seed_and_pull "${nonce}" "${repo}")"

	local manifest layers config manifest_dgst
	manifest="$(manifest_body "${repo}" "${nonce}")"
	mapfile -t layers < <(jq -r '.layers[].digest' <<<"${manifest}")
	config="$(jq -r '.config.digest' <<<"${manifest}")"
	manifest_dgst="$(manifest_digest "${repo}" "${nonce}")"
	[ "${#layers[@]}" -eq "${LAYER_COUNT}" ] || fail "seed has ${#layers[@]} layers, want ${LAYER_COUNT}"

	local d
	for d in "${layers[@]}"; do
		content_has "${d}" && fail "layer present before run: ${d}"
	done

	# docker create pulls the image (fast path) and prepares the container root
	# filesystem (composefs mount) without running a command, which the seeded
	# binary-less image could not do anyway.
	docker create --name e2e-run "${ref}" /nonexistent >/dev/null ||
		fail "docker create failed for ${ref}"

	# (a) zero layer fetch.
	for d in "${layers[@]}"; do
		content_has "${d}" && fail "layer blob fetched into content store: ${d}"
	done
	# (b) positive controls: the pull ran through this content store.
	content_has "${manifest_dgst}" || fail "manifest blob absent from content store"
	content_has "${config}" || fail "config blob absent from content store"

	# (c) shared image metadata mount: preparing the container mounted the
	# image's EROFS once at its chain snapshot. It carries the merged tree's
	# metadata only — file content is served through the object directory each
	# container's overlay attaches as a data-only lower — and is immutable.
	local chainfs fstype
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "no shared image mount found for the image"
	fstype="$(findmnt -no FSTYPE "${chainfs}")"
	[ "${fstype}" = "erofs" ] || fail "chain mount ${chainfs} is ${fstype}, want erofs (image metadata)"
	local layer_file="${chainfs}/layer-1.txt"
	[ -f "${layer_file}" ] || fail "metadata mount missing layer entry at ${layer_file}"
	if touch "${chainfs}/scratch.txt" 2>/dev/null; then
		fail "shared image mount is writable"
	fi

	# (d) consumer-executed writable overlay: docker mounted the returned
	# overlay spec to write the container's init files, which landed in a
	# snapshot upper directory.
	local hosts
	hosts="$(find /var/lib/containerd-cfs-grpc/snapshots -path '*/upper/etc/hosts' -print -quit 2>/dev/null)"
	[ -n "${hosts}" ] || fail "no init-layer write found in any snapshot upper"

	docker rm -f e2e-run >/dev/null || true
	echo "PASS: zero-fetch create (shared metadata mount, layer entries, init upper)"
}

# assert_docker_run is the milestone check: a knarr-pulled image must actually
# execute under docker run, through the composefs mount, without any layer
# entering containerd's content store. docker interposes a committed init layer
# between the image and the container, so this also exercises the layered
# overlay (init upper over the composefs mount).
assert_docker_run() {
	local nonce="3${RANDOM}${RANDOM}"
	local repo="cfs/e2e-run" ref out
	ref="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "${repo}")"

	# The seeded binary writes a file into the container root before printing,
	# so a passing run also proves the rootfs is writable inside the container.
	out="$(docker run --rm --network=none "${ref}")" ||
		fail "docker run failed for ${ref}"
	[ "${out}" = "cfs hello ${nonce}" ] ||
		fail "docker run output = '${out}', want 'cfs hello ${nonce}'"

	local manifest d
	manifest="$(manifest_body "${repo}" "${nonce}")"
	while read -r d; do
		content_has "${d}" && fail "layer blob fetched into content store: ${d}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")

	echo "PASS: docker run executes from the composefs-served rootfs (writable, zero layer fetch)"
}

# assert_lifecycle_cleanup covers the container lifecycle: a container's
# writable upper persists across stop/start of that container while a fresh
# container from the same image starts clean; docker stop delivers SIGTERM
# through the shim (clean exit 0); docker rm removes the container while the
# image's shared composefs mount stays for reuse; and docker rmi tears the
# image's snapshots — and with them the mount — down.
assert_lifecycle_cleanup() {
	local nonce="4${RANDOM}${RANDOM}"
	local repo="cfs/e2e-life" ref
	ref="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "${repo}")"

	# Write on the first run, verify on a restart of the same container, and
	# expect a clean upper in a brand-new container.
	local out
	out="$(docker run --network=none --name e2e-persist "${ref}" /hello persist)" ||
		fail "docker run (persist write) failed for ${ref}"
	[ "${out}" = "persist wrote" ] ||
		fail "persist write output = '${out}', want 'persist wrote'"
	out="$(docker start -a e2e-persist)" ||
		fail "docker start of the stopped persist container failed"
	[ "${out}" = "persist ok" ] ||
		fail "restart output = '${out}', want 'persist ok' (upper did not persist)"
	docker rm e2e-persist >/dev/null || fail "docker rm (persist) failed"
	out="$(docker run --rm --network=none "${ref}" /hello persist)" ||
		fail "docker run (fresh persist) failed"
	[ "${out}" = "persist wrote" ] ||
		fail "fresh container output = '${out}', want 'persist wrote' (upper not clean)"

	docker run -d --network=none --name e2e-daemon "${ref}" /hello sleep >/dev/null ||
		fail "docker run -d failed for ${ref}"
	[ "$(docker inspect -f '{{.State.Running}}' e2e-daemon)" = "true" ] ||
		fail "daemon container is not running"
	local chainfs
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "no shared image mount for the daemon image"

	docker stop e2e-daemon >/dev/null || fail "docker stop failed"
	[ "$(docker inspect -f '{{.State.ExitCode}}' e2e-daemon)" = "0" ] ||
		fail "daemon did not exit cleanly on SIGTERM"
	docker rm e2e-daemon >/dev/null || fail "docker rm failed"
	findmnt "${chainfs}" >/dev/null 2>&1 ||
		fail "shared composefs mount vanished with the container"

	docker rmi "${ref}" >/dev/null || fail "docker rmi failed"
	# Snapshot removal runs through containerd's garbage collector, so give the
	# unmount a moment.
	local _
	for _ in $(seq 1 30); do
		findmnt "${chainfs}" >/dev/null 2>&1 || break
		sleep 1
	done
	if findmnt "${chainfs}" >/dev/null 2>&1; then
		fail "shared composefs mount survived image removal"
	fi
	echo "PASS: upper persists across restart, fresh container clean; stop/rm keep the shared mount; rmi tears it down"
}

# assert_real_image drives the same flow with a real-world image straight from
# its home registry, digest-pinned like an OTA agent would: crane exports the
# image as an OCI layout, knarr materializes it out of band, and docker runs it
# by digest — the registry serves only the manifest and config while every
# layer comes from the local store. A real image brings what the synthetic
# seeds cannot: registry media types, a multi-arch index resolved per platform,
# and a shell for an in-container write.
assert_real_image() {
	local image="${REAL_IMAGE}" layout="${work}/real-image" repo digest out
	repo="${image#*/}"
	repo="${repo%%[:@]*}"

	crane pull --format=oci --platform "linux/$(go env GOARCH)" "${image}" "${layout}" ||
		fail "crane pull of ${image} failed"
	digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	"${KNARR_BIN}" pull --from-dir "${layout}" "${repo}@${digest}" ||
		fail "knarr pull of ${repo}@${digest} failed"

	local pinned="${image%%[:@]*}@${digest}"
	out="$(docker run --rm --network=none "${pinned}" /bin/sh -c 'echo "test write" >/somefile && cat /somefile')" ||
		fail "docker run of ${pinned} failed"
	[ "${out}" = "test write" ] ||
		fail "real-image run output = '${out}', want 'test write'"

	# Zero layer fetch: the layers named by the pinned manifest never entered
	# containerd's content store; docker fetched only manifest and config.
	local manifest d
	manifest="$(cat "${layout}/blobs/sha256/${digest#sha256:}")"
	while read -r d; do
		content_has "${d}" && fail "real-image layer fetched into content store: ${d}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	content_has "$(jq -r '.config.digest' <<<"${manifest}")" ||
		fail "real-image config absent from content store"

	docker rmi "${pinned}" >/dev/null || fail "docker rmi of ${pinned} failed"
	echo "PASS: real image ${image} runs digest-pinned with zero layer fetch"
}

assert_fail_closed() {
	# Seed an image but do NOT pull it into the knarr store; the fast path has
	# nothing to serve, so the pull must fail rather than fetch layers.
	local nonce="2${RANDOM}${RANDOM}"
	local ref="${REGISTRY}/cfs/e2e-miss:${nonce}"
	SEED_LAYER_COUNT="${LAYER_COUNT}" SEED_NONCE="${nonce}" \
		"${SCRIPT_DIR}/seed-registry.sh" "${ref}" >&2
	# The snapshotter's error is not logged by any daemon; it travels over gRPC
	# through containerd's unpacker into the pull error, so assert on that.
	local out
	if out="$(docker pull "${ref}" 2>&1)"; then
		fail "docker pull of an unmaterialized image succeeded, want fail-closed"
	fi
	grep -q "not in the local image store" <<<"${out}" ||
		fail "pull failed without the snapshotter's fail-closed error: ${out}"
	echo "PASS: unmaterialized image fails closed"
}

assert_verity_seals_store() {
	# A store object is fs-verity sealed and therefore read-only; a write attempt
	# must be rejected.
	local object
	object="$(find "${KNARR_ROOT}" -type f -path '*objects*' -print -quit 2>/dev/null)"
	[ -n "${object}" ] || fail "no store object found to test fs-verity"
	if printf 'x' 2>/dev/null >>"${object}"; then
		fail "wrote to fs-verity object ${object}; sealing is not in effect"
	fi
	echo "PASS: fs-verity seals store objects (write rejected)"
}

assert_restart_recovers() {
	local ref="$1"
	local pidfile=/var/log/cfs-devcontainer/containerd-cfs-grpc.pid
	if [ -s "${pidfile}" ]; then
		kill "$(cat "${pidfile}")" 2>/dev/null || true
	fi
	CFS_FORCE_RESTART=1 "${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		fail "run-cfs.sh restart failed"
	# run-cfs may restart containerd for proxy re-registration, so retry until
	# docker has reconnected to it.
	local created=0 _
	for _ in $(seq 1 30); do
		if docker create --name e2e-restart "${ref}" /nonexistent >/dev/null 2>&1; then
			created=1
			break
		fi
		docker rm -f e2e-restart >/dev/null 2>&1 || true
		sleep 1
	done
	[ "${created}" = 1 ] ||
		fail "docker create after restart failed (index did not rebuild or docker did not reconnect)"
	docker rm -f e2e-restart >/dev/null || true
	echo "PASS: snapshotter recovers after restart"
}

main() {
	work="$(mktemp -d)"
	trap cleanup EXIT
	build_knarr

	echo "runtime versions:"
	docker --version
	containerd --version
	"${KNARR_BIN}" --version 2>/dev/null || true

	# Reuse the last fast-path image for the restart check.
	local nonce="9${RANDOM}${RANDOM}"
	local restart_ref
	restart_ref="$(seed_and_pull "${nonce}" "cfs/e2e-restart-img")"

	assert_fastpath_run
	assert_docker_run
	assert_lifecycle_cleanup
	assert_real_image
	assert_fail_closed
	assert_verity_seals_store
	assert_restart_recovers "${restart_ref}"

	echo "E2E PASS: knarr-fed zero-fetch composefs create and run, lifecycle cleanup,"
	echo "digest-pinned real image, fail-closed miss, fs-verity sealing, and restart"
	echo "recovery all hold."
}

main "$@"
