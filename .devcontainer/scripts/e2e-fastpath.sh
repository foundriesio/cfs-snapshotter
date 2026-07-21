#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

CONTAINERD_SOCKET=/run/containerd/containerd.sock
DOCKER_NAMESPACE=moby
REGISTRY=127.0.0.1:5000
KNARR_ROOT=/var/lib/knarr
KNARR_BIN=${KNARR_BIN:-/opt/cfs-dev/bin/knarr}
CFS_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log

work=""
image_ref=""

cleanup() {
	local name
	for name in cfs-e2e-1 cfs-e2e-2 cfs-e2e-recovery; do
		if docker container inspect "${name}" >/dev/null 2>&1; then
			docker rm -f "${name}" >/dev/null 2>&1 || echo "warning: failed to remove ${name}" >&2
		fi
	done
	if [ -n "${image_ref}" ] && docker image inspect "${image_ref}" >/dev/null 2>&1; then
		docker rmi -f "${image_ref}" >/dev/null 2>&1 || echo "warning: failed to remove ${image_ref}" >&2
	fi
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
}

fail() {
	echo "E2E FAIL: $1" >&2
	tail -80 "${CFS_LOG}" >&2 2>/dev/null || echo "warning: failed to read ${CFS_LOG}" >&2
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		snapshots --snapshotter cfs ls >&2 2>/dev/null || echo "warning: failed to list cfs snapshots" >&2
	exit 1
}

content_has() {
	local digest="$1" listing
	listing="$(ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" content ls -q)" ||
		fail "cannot list containerd content"
	grep -Fxq "${digest}" <<<"${listing}"
}

chain_mount() {
	awk '$5 ~ /containerd-cfs-grpc\/snapshots\/[0-9]+\/fs$/ {print $5}' \
		/proc/self/mountinfo | tail -1
}

chain_mounts() {
	awk '$5 ~ /containerd-cfs-grpc\/snapshots\/[0-9]+\/fs$/ {n++} END {print n + 0}' \
		/proc/self/mountinfo
}

wait_unmounted() {
	local target="$1"
	for _ in $(seq 1 30); do
		if ! findmnt -M "${target}" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}

seed_and_stage() {
	local nonce="$1" repo="cfs/e2e" layout="${work}/layout" digest
	image_ref="${REGISTRY}/${repo}:${nonce}"
	SEED_LAYER_COUNT=3 SEED_NONCE="${nonce}" SEED_RUNNABLE=1 SEED_KEEP_LAYOUT="${layout}" \
		"${SCRIPT_DIR}/seed-registry.sh" "${image_ref}" >&2
	digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	"${KNARR_BIN}" pull --from-dir "${layout}" "${repo}@${digest}" >&2 ||
		fail "knarr pull failed"
}

assert_real_zero_fetch_run() {
	local nonce="$1" layout="${work}/layout" manifest_digest manifest config output digest
	manifest_digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	manifest="$(cat "${layout}/blobs/sha256/${manifest_digest#sha256:}")"
	config="$(jq -r '.config.digest' <<<"${manifest}")"

	while read -r digest; do
		content_has "${digest}" && fail "layer was present before pull: ${digest}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	docker pull "${image_ref}" >/dev/null || fail "docker pull failed"
	output="$(docker run --rm --network=none "${image_ref}")" || fail "docker run failed"
	[ "${output}" = "cfs hello ${nonce}" ] || fail "unexpected container output: ${output}"

	while read -r digest; do
		content_has "${digest}" && fail "layer was fetched: ${digest}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	content_has "${config}" || fail "image config was not fetched"
	echo "PASS: knarr pull -> zero-fetch docker run served real image content"
}

assert_concurrent_shared_mount() {
	local chainfs="$1" before after pid fstype
	before="$(chain_mounts)"
	docker run -d --network=none --name cfs-e2e-1 "${image_ref}" /hello sleep >/dev/null &
	local run1=$!
	docker run -d --network=none --name cfs-e2e-2 "${image_ref}" /hello sleep >/dev/null &
	local run2=$!
	wait "${run1}" || fail "first concurrent docker run failed"
	wait "${run2}" || fail "second concurrent docker run failed"
	after="$(chain_mounts)"
	[ "${after}" -eq "${before}" ] || fail "concurrent containers created another image mount"
	[ "$(findmnt -n -o FSTYPE -M "${chainfs}")" = erofs ] || fail "shared image mount is not EROFS"

	local name
	for name in cfs-e2e-1 cfs-e2e-2; do
		pid="$(docker inspect -f '{{.State.Pid}}' "${name}")"
		fstype="$(findmnt -N "${pid}" -n -o FSTYPE --target /)"
		[ "${fstype}" = overlay ] || fail "${name} rootfs is ${fstype}, want overlay"
	done
	docker rm -f cfs-e2e-1 cfs-e2e-2 >/dev/null || fail "remove concurrent containers"
	echo "PASS: concurrent containers shared one EROFS mount and used two flat overlays"
}

assert_mount_recovery() {
	local nonce="$1" chainfs="$2" output
	docker create --name cfs-e2e-recovery "${image_ref}" >/dev/null || fail "create recovery container"
	umount "${chainfs}" || fail "unmount shared image mount"
	findmnt -M "${chainfs}" >/dev/null 2>&1 && fail "shared image mount survived explicit unmount"
	CFS_FORCE_RESTART=1 "${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "restart snapshotter"
	output="$(docker start -a cfs-e2e-recovery)" || fail "start existing container after mount loss"
	[ "${output}" = "cfs hello ${nonce}" ] || fail "recovered container output: ${output}"
	[ "$(findmnt -n -o FSTYPE -M "${chainfs}")" = erofs ] || fail "Mounts did not restore EROFS mount"
	docker rm cfs-e2e-recovery >/dev/null || fail "remove recovery container"
	echo "PASS: an existing container recovered its image mount through Mounts"
}

assert_gc_teardown() {
	local chainfs="$1" loopdev
	loopdev="$(findmnt -n -o SOURCE -M "${chainfs}")"
	docker rmi "${image_ref}" >/dev/null || fail "docker rmi failed"
	wait_unmounted "${chainfs}" || fail "image mount survived garbage collection"
	if grep -q '/containerd-cfs-grpc/snapshots/rm-[^ /]*/fs' /proc/self/mountinfo; then
		fail "renamed snapshot mount residue remains in mountinfo"
	fi
	if losetup "${loopdev}" >/dev/null 2>&1; then
		fail "image loop device ${loopdev} remains attached"
	fi
	echo "PASS: image GC removed the chain mount, loop device, and rm-* mount residue"
}

find_hello_object() {
	local layout="${work}/layout" manifest_digest layer object="" candidate
	manifest_digest="$(jq -r '.manifests[0].digest' "${layout}/index.json")"
	layer="$(jq -r '.layers[0].digest' "${layout}/blobs/sha256/${manifest_digest#sha256:}")"
	tar -xzOf "${layout}/blobs/sha256/${layer#sha256:}" ./hello >"${work}/hello" ||
		fail "extract seeded /hello"
	while IFS= read -r candidate; do
		if cmp -s "${candidate}" "${work}/hello"; then
			object="${candidate}"
			break
		fi
	done < <(find "${KNARR_ROOT}" -type f -size "$(stat -c%s "${work}/hello")c")
	[ -n "${object}" ] || fail "cannot find /hello in knarr objects"
	printf '%s\n' "${object}"
}

assert_tamper_fails_at_open() {
	local object output tamper_chain
	object="$(find_hello_object)"
	rm -f "${object}" || fail "unlink /hello object"
	printf 'tampered\n' >"${object}" || fail "replace /hello object"
	if output="$(docker run --rm --network=none "${image_ref}" 2>&1)"; then
		fail "container ran with a tampered backing object"
	fi
	grep -Eqi 'input/output error|\bEIO\b' <<<"${output}" ||
		fail "tampered run did not report the expected read failure: ${output}"
	tamper_chain="$(chain_mount)"
	[ -n "${tamper_chain}" ] || fail "tampered run failed before mounting image metadata"
	[ "$(findmnt -n -o FSTYPE -M "${tamper_chain}")" = erofs ] ||
		fail "tampered run failed before establishing EROFS metadata mount"
	docker rmi "${image_ref}" >/dev/null || fail "remove image after tamper check"
	wait_unmounted "${tamper_chain}" || fail "tamper-check image mount survived cleanup"
	echo "PASS: verity rejected the tampered object when overlayfs opened /hello"
}

main() {
	[ -x "${KNARR_BIN}" ] || fail "knarr CLI not found at ${KNARR_BIN}"
	work="$(mktemp -d)"
	trap cleanup EXIT
	local nonce="${RANDOM}${RANDOM}" chainfs
	seed_and_stage "${nonce}"
	assert_real_zero_fetch_run "${nonce}"
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "no shared image mount found"
	assert_concurrent_shared_mount "${chainfs}"
	assert_mount_recovery "${nonce}" "${chainfs}"
	assert_gc_teardown "${chainfs}"
	assert_tamper_fails_at_open
	echo "E2E PASS: real content, recovery, concurrency, teardown, and tamper enforcement"
}

main "$@"
