#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

# Drives the full fast path end to end: pull a multi-layer image into the knarr
# store out of band, create and run containers from it through docker, and
# assert that the snapshotter served them from a content-addressed composefs
# mount without fetching any layer, that a container executes and prints from
# that rootfs, that writes land in the overlay while the image stays immutable,
# that a container's writes persist across its stop/start while a fresh
# container starts clean, that stop/rm/rmi tear the container and then the
# image mount down, that concurrent containers share one image mount with
# isolated writes, that an image update sharing layers with its predecessor
# runs zero-fetch and survives the old version's removal, that a real-world
# image runs digest-pinned from its home registry with only manifest and
# config fetched, that an unmaterialized image fails closed, that a tampered
# store object is rejected by fs-verity and breaks a run, and that the
# snapshotter recovers after a restart.

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
CFS_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log
# Real-world image for the registry round-trip check; needs network access to
# its home registry (manifest and config only — layers must not be fetched).
REAL_IMAGE="${CFS_E2E_REAL_IMAGE:-docker.io/library/busybox:latest}"

work=""

cleanup() {
	docker rm -f e2e-run e2e-persist e2e-daemon \
		e2e-shared-1 e2e-shared-2 e2e-shared-3 e2e-restart >/dev/null 2>&1 || true
	# Drop the images too, so a failed or repeated run does not leave snapshots
	# and shared mounts that skew the next run's mount-count baselines.
	docker images --filter "reference=${REGISTRY}/cfs/*" -q 2>/dev/null |
		sort -u | xargs -r docker rmi -f >/dev/null 2>&1 || true
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

require_knarr() {
	# The knarr CLI is built on the host from the pinned module and mounted at
	# KNARR_BIN; the container has no credentials to fetch the internal module
	# itself. Fail early with a clear message if the mount is missing.
	[ -x "${KNARR_BIN}" ] ||
		fail "knarr CLI not found at ${KNARR_BIN}; devshell.sh builds it on the host"
	echo "using knarr CLI at ${KNARR_BIN}"
}

# chain_mount prints the newest shared image metadata mount: the snapshotter mounts
# each image once, at its chain snapshot's fs directory, and containers overlay
# on top of it.
chain_mount() {
	awk '$2 ~ /containerd-cfs-grpc\/snapshots\/[0-9]+\/fs$/ {print $2}' /proc/self/mounts | tail -1
}

# chain_mounts counts the live shared image metadata mounts.
chain_mounts() {
	awk '$2 ~ /containerd-cfs-grpc\/snapshots\/[0-9]+\/fs$/ {n++} END {print n + 0}' /proc/self/mounts
}

# seed_and_pull seeds a fresh multi-layer image into the registry, keeps its OCI
# layout, and pulls it into the knarr store by digest. Prints the image ref.
# An optional third argument overrides the layer count.
seed_and_pull() {
	local nonce="$1" repo="$2" layers="${3:-${LAYER_COUNT}}" ref layout digest
	ref="${REGISTRY}/${repo}:${nonce}"
	layout="${work}/${repo//\//_}-${nonce}"
	SEED_LAYER_COUNT="${layers}" SEED_NONCE="${nonce}" SEED_KEEP_LAYOUT="${layout}" \
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

# assert_shared_mount_concurrency covers the shared-mount claim: any number of
# containers of one image share a single composefs mount, their writes stay
# isolated in per-container uppers, and removing one container leaves its
# siblings and the shared mount untouched.
assert_shared_mount_concurrency() {
	local nonce="5${RANDOM}${RANDOM}"
	local repo="cfs/e2e-shared" ref before after out i
	ref="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "${repo}")"

	before="$(chain_mounts)"
	# Start the containers concurrently so their snapshot Prepares race on the
	# shared chain mount.
	local pids=()
	for i in 1 2 3; do
		docker run -d --network=none --name "e2e-shared-${i}" "${ref}" /hello sleep >/dev/null &
		pids+=("$!")
	done
	for i in "${!pids[@]}"; do
		wait "${pids[$i]}" || fail "docker run -d e2e-shared-$((i + 1)) failed"
	done
	after="$(chain_mounts)"
	[ "$((after - before))" -eq 1 ] ||
		fail "3 containers added $((after - before)) composefs mounts, want exactly 1 shared"

	# The first persist write in each container lands in its own upper: a
	# repeat in the same container sees it, a sibling does not.
	out="$(docker exec e2e-shared-1 /hello persist)" || fail "exec in e2e-shared-1 failed"
	[ "${out}" = "persist wrote" ] || fail "container 1 first write output '${out}'"
	out="$(docker exec e2e-shared-1 /hello persist)" || fail "exec recheck in e2e-shared-1 failed"
	[ "${out}" = "persist ok" ] || fail "container 1 lost its own write ('${out}')"
	out="$(docker exec e2e-shared-2 /hello persist)" || fail "exec in e2e-shared-2 failed"
	[ "${out}" = "persist wrote" ] || fail "container 2 saw container 1's write ('${out}')"

	docker rm -f e2e-shared-1 >/dev/null || fail "docker rm e2e-shared-1 failed"
	[ "$(docker inspect -f '{{.State.Running}}' e2e-shared-3)" = "true" ] ||
		fail "sibling stopped by removing e2e-shared-1"
	out="$(docker exec e2e-shared-2 /hello persist)" || fail "exec after sibling removal failed"
	[ "${out}" = "persist ok" ] || fail "container 2 lost its rootfs after sibling removal ('${out}')"
	[ "$(chain_mounts)" -eq "${after}" ] ||
		fail "shared mount count changed on container removal"

	docker rm -f e2e-shared-2 e2e-shared-3 >/dev/null
	docker rmi "${ref}" >/dev/null || fail "docker rmi failed"
	# Wait for containerd's garbage collector to settle the mount count so the
	# next check starts from a stable baseline.
	local _
	for _ in $(seq 1 30); do
		[ "$(chain_mounts)" -le "${before}" ] && break
		sleep 1
	done
	echo "PASS: 3 containers share one composefs mount with isolated writes"
}

# assert_update_flow covers an OTA-style image update: v2 shares v1's layers
# (same nonce, one layer more, so the blobs are byte-identical), runs
# zero-fetch over the shared prefix chains, survives removal of the old
# version, and removing both versions tears every mount down.
assert_update_flow() {
	local nonce="6${RANDOM}${RANDOM}"
	local ref1 ref2 before out
	before="$(chain_mounts)"
	# Chain identity is layer-based, so distinct repos still share chains.
	ref1="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "cfs/e2e-v1")"
	ref2="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "cfs/e2e-v2" 4)"

	# Guard the premise: the seeds must really share their first layer.
	local l1a l1b
	l1a="$(manifest_body "cfs/e2e-v1" "${nonce}" | jq -r '.layers[0].digest')"
	l1b="$(manifest_body "cfs/e2e-v2" "${nonce}" | jq -r '.layers[0].digest')"
	[ "${l1a}" = "${l1b}" ] ||
		fail "seed layers not shared between versions (non-reproducible build?)"

	out="$(docker run --rm --network=none "${ref1}")" || fail "docker run v1 failed"
	[ "${out}" = "cfs hello ${nonce}" ] || fail "v1 output '${out}'"
	out="$(docker run --rm --network=none "${ref2}")" || fail "docker run v2 failed"
	[ "${out}" = "cfs hello ${nonce}" ] || fail "v2 output '${out}'"

	# Zero fetch for the update: neither the shared prefix layers nor v2's new
	# top layer entered the content store.
	local manifest d
	manifest="$(manifest_body "cfs/e2e-v2" "${nonce}")"
	while read -r d; do
		content_has "${d}" && fail "update layer fetched into content store: ${d}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")

	docker rmi "${ref1}" >/dev/null || fail "docker rmi v1 failed"
	out="$(docker run --rm --network=none "${ref2}")" || fail "docker run v2 after v1 removal failed"
	[ "${out}" = "cfs hello ${nonce}" ] || fail "v2 broken after v1 removal ('${out}')"

	docker rmi "${ref2}" >/dev/null || fail "docker rmi v2 failed"
	local _
	for _ in $(seq 1 30); do
		[ "$(chain_mounts)" -le "${before}" ] && break
		sleep 1
	done
	[ "$(chain_mounts)" -le "${before}" ] ||
		fail "composefs mounts leaked after removing both versions"
	echo "PASS: v2 update shares v1's chains zero-fetch and survives v1 removal"
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

# assert_tampered_object_fails_run upgrades the write-rejection check to full
# enforcement: root cannot write to a sealed object but can unlink and
# recreate it with different bytes, and a container run over the tampered
# store must fail — composefs verity=require refuses to serve a backing
# object that no longer carries the recorded fs-verity digest. The image is
# removed before tampering so the run gets a fresh mount: a live mount keeps
# the original (still-sealed) inode of an unlinked object alive and would
# serve the verified bytes, never the tampered file.
assert_tampered_object_fails_run() {
	local nonce="7${RANDOM}${RANDOM}"
	local repo="cfs/e2e-tamper" ref layout digest l1 before out
	before="$(chain_mounts)"
	ref="$(SEED_RUNNABLE=1 seed_and_pull "${nonce}" "${repo}")"
	layout="${work}/${repo//\//_}-${nonce}"

	out="$(docker run --rm --network=none "${ref}")" || fail "pre-tamper run failed"
	[ "${out}" = "cfs hello ${nonce}" ] || fail "pre-tamper output '${out}'"

	# Locate the store object backing /hello: extract the binary from the seed
	# layout's first layer and match store objects by content.
	digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	l1="$(jq -r '.layers[0].digest' "${layout}/blobs/sha256/${digest#sha256:}")"
	tar -xzOf "${layout}/blobs/sha256/${l1#sha256:}" ./hello >"${work}/hello.bin" ||
		fail "cannot extract /hello from the seed layer"
	local obj="" f
	while IFS= read -r f; do
		if cmp -s "${f}" "${work}/hello.bin"; then
			obj="${f}"
			break
		fi
	done < <(find "${KNARR_ROOT}" -type f -size "$(stat -c%s "${work}/hello.bin")c")
	[ -n "${obj}" ] || fail "store object for /hello not found"

	# Drop the image so the composefs mount (and its cached object inodes) is
	# torn down before the store is tampered with.
	docker rmi "${ref}" >/dev/null || fail "docker rmi before tampering failed"
	local _
	for _ in $(seq 1 30); do
		[ "$(chain_mounts)" -le "${before}" ] && break
		sleep 1
	done
	[ "$(chain_mounts)" -le "${before}" ] || fail "composefs mount not released before tampering"

	rm -f "${obj}" || fail "cannot unlink the store object"
	printf 'tampered' >"${obj}" || fail "cannot plant the tampered object"

	if out="$(docker run --rm --network=none "${ref}" 2>&1)"; then
		fail "container ran from a tampered store (output '${out}')"
	fi
	docker rmi "${ref}" >/dev/null 2>&1 || true
	echo "PASS: tampered store object breaks the run (verity enforced at open)"
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
	require_knarr

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
	assert_shared_mount_concurrency
	assert_update_flow
	assert_real_image
	assert_fail_closed
	assert_verity_seals_store
	assert_tampered_object_fails_run
	assert_restart_recovers "${restart_ref}"

	echo "E2E PASS: knarr-fed zero-fetch composefs create and run, lifecycle cleanup,"
	echo "digest-pinned real image, fail-closed miss, fs-verity sealing, and restart"
	echo "recovery all hold."
}

main "$@"
