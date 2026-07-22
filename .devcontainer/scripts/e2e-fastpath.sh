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
unheld_ref=""
base_ref=""
substitution_ref=""

cleanup() {
	local name
	for name in cfs-e2e-1 cfs-e2e-2 cfs-e2e-recovery; do
		if docker container inspect "${name}" >/dev/null 2>&1; then
			docker rm -f "${name}" >/dev/null 2>&1 || echo "warning: failed to remove ${name}" >&2
		fi
	done
	local ref
	for ref in "${image_ref}" "${unheld_ref}" "${base_ref}" "${substitution_ref}"; do
		if [ -n "${ref}" ] && docker image inspect "${ref}" >/dev/null 2>&1; then
			docker rmi -f "${ref}" >/dev/null 2>&1 || echo "warning: failed to remove ${ref}" >&2
		fi
	done
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

forget_knarr_images() {
	local listing refs ref
	listing="$("${KNARR_BIN}" ls --format json)" || fail "cannot list staged knarr images"
	refs="$(jq -r '.[].ref' <<<"${listing}")" || fail "cannot parse staged knarr images"
	while IFS= read -r ref; do
		[ -z "${ref}" ] && continue
		"${KNARR_BIN}" remove "${ref}" >&2 || fail "cannot forget staged knarr image ${ref}"
	done <<<"${refs}"
}

seed_image() {
	local nonce="$1" ref="$2" layer_count="$3" layout="$4" alternate_config="${5:-}"
	SEED_LAYER_COUNT="${layer_count}" SEED_NONCE="${nonce}" SEED_RUNNABLE=1 \
		SEED_ALTERNATE_CONFIG="${alternate_config}" \
		SEED_KEEP_LAYOUT="${layout}" "${SCRIPT_DIR}/seed-registry.sh" "${ref}" >&2 ||
		fail "cannot seed ${ref}"
}

manifest_digest() {
	jq -r '.manifests[0].digest' "${1}/index.json"
}

pinned_ref() {
	local repo="$1" layout="$2" digest
	digest="$(manifest_digest "${layout}")" || return 1
	printf '%s@%s\n' "${repo}" "${digest}"
}

stage_image() {
	local repo="$1" layout="$2" digest
	digest="$(manifest_digest "${layout}")" ||
		fail "cannot read manifest digest for ${repo}"
	"${KNARR_BIN}" pull --from-dir "${layout}" "${repo}@${digest}" >&2 ||
		fail "knarr pull failed for ${repo}"
}

image_layers() {
	local layout="$1" manifest
	manifest="$(manifest_digest "${layout}")" || return 1
	jq -c '[.layers[].digest]' "${layout}/blobs/sha256/${manifest#sha256:}"
}

image_config_digest() {
	local layout="$1" manifest
	manifest="$(manifest_digest "${layout}")" || return 1
	jq -r '.config.digest' "${layout}/blobs/sha256/${manifest#sha256:}"
}

first_diff_id() {
	local layout="$1" manifest config
	manifest="$(manifest_digest "${layout}")" || return 1
	config="$(jq -r '.config.digest' "${layout}/blobs/sha256/${manifest#sha256:}")" || return 1
	jq -r '.rootfs.diff_ids[0]' "${layout}/blobs/sha256/${config#sha256:}"
}

seed_and_stage() {
	local nonce="$1" repo="cfs/e2e" layout="${work}/layout" tag_ref
	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}" "${tag_ref}" 3 "${layout}"
	stage_image "${repo}" "${layout}"
	image_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin staged image ref"
}

assert_handshake_identity() {
	local nonce="$1" pair_nonce="${nonce}-pair"
	local base_repo="cfs/e2e-base" derived_repo="cfs/e2e-derived"
	local substitution_repo="cfs/e2e-substitution" unheld_repo="cfs/e2e-unheld"
	local base_layout="${work}/base-layout" derived_layout="${work}/derived-layout"
	local substitution_layout="${work}/substitution-layout" unheld_layout="${work}/unheld-layout"
	local base_tag derived_tag substitution_tag unheld_tag
	local base_first derived_first unheld_first derived_layers substitution_layers
	local derived_config substitution_config output

	base_tag="${REGISTRY}/${base_repo}:${nonce}"
	derived_tag="${REGISTRY}/${derived_repo}:${nonce}"
	substitution_tag="${REGISTRY}/${substitution_repo}:${nonce}"
	unheld_tag="${REGISTRY}/${unheld_repo}:${nonce}"
	seed_image "${pair_nonce}" "${base_tag}" 1 "${base_layout}"
	seed_image "${pair_nonce}" "${derived_tag}" 2 "${derived_layout}"
	stage_image "${derived_repo}" "${derived_layout}"
	seed_image "${pair_nonce}" "${substitution_tag}" 2 "${substitution_layout}" 1
	seed_image "${nonce}-unheld" "${unheld_tag}" 1 "${unheld_layout}"

	base_ref="$(pinned_ref "${REGISTRY}/${base_repo}" "${base_layout}")" || fail "cannot pin base ref"
	substitution_ref="$(pinned_ref "${REGISTRY}/${substitution_repo}" "${substitution_layout}")" ||
		fail "cannot pin config-substitution ref"
	unheld_ref="$(pinned_ref "${REGISTRY}/${unheld_repo}" "${unheld_layout}")" || fail "cannot pin unheld ref"

	base_first="$(first_diff_id "${base_layout}")" || fail "cannot read base diffID"
	derived_first="$(first_diff_id "${derived_layout}")" || fail "cannot read derived diffID"
	unheld_first="$(first_diff_id "${unheld_layout}")" || fail "cannot read unheld diffID"
	[ "${base_first}" = "${derived_first}" ] || fail "base is not a prefix of the staged derived image"
	[ "${unheld_first}" != "${derived_first}" ] || fail "unheld image shares the staged image's base layer"
	derived_layers="$(image_layers "${derived_layout}")" || fail "cannot read staged image layers"
	substitution_layers="$(image_layers "${substitution_layout}")" || fail "cannot read substitution image layers"
	[ "${derived_layers}" = "${substitution_layers}" ] || fail "config-substitution image changed the layer set"
	derived_config="$(image_config_digest "${derived_layout}")" || fail "cannot read staged image config"
	substitution_config="$(image_config_digest "${substitution_layout}")" || fail "cannot read substitution image config"
	[ "${derived_config}" != "${substitution_config}" ] || fail "config-substitution image reused the staged config"

	if output="$(docker pull "${substitution_ref}" 2>&1)"; then
		fail "docker pull succeeded for an unheld config substitution: ${output}"
	fi
	grep -Fq 'is not present in the local store' <<<"${output}" ||
		fail "config-substitution pull did not fail at the identity gate: ${output}"
	echo "PASS: an unheld config substitution with identical layers failed during docker pull"

	if output="$(docker pull "${unheld_ref}" 2>&1)"; then
		fail "docker pull succeeded for an image absent from knarr: ${output}"
	fi
	grep -Fq 'is not present in the local store' <<<"${output}" ||
		fail "unheld pull did not fail at the handshake identity gate: ${output}"
	echo "PASS: an unrelated image absent from knarr failed during docker pull"

	if output="$(docker pull "${base_ref}" 2>&1)"; then
		fail "docker pull succeeded for an unheld base prefix: ${output}"
	fi
	grep -Fq 'is not present in the local store' <<<"${output}" ||
		fail "base-prefix pull did not fail at the handshake identity gate: ${output}"
	echo "PASS: an unheld base prefix failed during docker pull"
}

assert_real_zero_fetch_run() {
	local nonce="$1" layout="${work}/layout" mdigest manifest config output digest
	mdigest="$(manifest_digest "${layout}")" || fail "cannot read manifest digest"
	manifest="$(cat "${layout}/blobs/sha256/${mdigest#sha256:}")"
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
	local layout="${work}/layout" mdigest layer object="" candidate
	mdigest="$(manifest_digest "${layout}")" || fail "cannot read manifest digest"
	layer="$(jq -r '.layers[0].digest' "${layout}/blobs/sha256/${mdigest#sha256:}")"
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
	forget_knarr_images
	assert_handshake_identity "${nonce}"
	seed_and_stage "${nonce}"
	assert_real_zero_fetch_run "${nonce}"
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "no shared image mount found"
	assert_concurrent_shared_mount "${chainfs}"
	assert_mount_recovery "${nonce}" "${chainfs}"
	assert_gc_teardown "${chainfs}"
	assert_tamper_fails_at_open
	echo "E2E PASS: handshake identity, real content, recovery, concurrency, teardown, and tamper enforcement"
}

main "$@"
