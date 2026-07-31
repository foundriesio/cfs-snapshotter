#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

CONTAINERD_SOCKET=/run/containerd/containerd.sock
DOCKER_NAMESPACE=moby
REGISTRY=127.0.0.1:5000
WARM_KNARR_ROOT=/var/lib/knarr
WARM_CFS_ROOT=/var/lib/containerd-cfs-grpc
COLD_KNARR_ROOT=/var/lib/knarr-cold
COLD_CFS_ROOT=/var/lib/containerd-cfs-grpc-cold
KNARR_BIN=${KNARR_BIN:-/opt/cfs-dev/bin/knarr}
CFS_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log
# Size of the last layer of the image the interrupt leg pulls. It is
# incompressible, so its transfer outlasts the poll waiting for the layers
# before it to be staged — the window the pull is interrupted in.
PARTIAL_LAYER_BYTES=134217728

work=""
# knarr_root and cfs_root name the stores the phase in progress runs against.
# The warm phase pins the out-of-band mode on the devcontainer's default
# stores; the cold phase runs the staged mode — the boot posture — with
# stores of its own, because the two work modes are never mixed on one
# device.
knarr_root="${WARM_KNARR_ROOT}"
cfs_root="${WARM_CFS_ROOT}"
image_ref=""
unheld_ref=""
base_ref=""
substitution_ref=""
cold_ref=""
share_ref=""
share_more_ref=""
partial_ref=""
pull_pid=""

cleanup() {
	# A failure inside the interrupted-pull leg can leave its background pull
	# running; it has to go before the images it references are removed.
	if [ -n "${pull_pid}" ] && kill -0 "${pull_pid}" 2>/dev/null; then
		kill "${pull_pid}" 2>/dev/null || echo "warning: failed to stop the background pull" >&2
		wait "${pull_pid}" 2>/dev/null || true
	fi
	local name
	for name in cfs-e2e-1 cfs-e2e-2 cfs-e2e-recovery cfs-e2e-cold; do
		if docker container inspect "${name}" >/dev/null 2>&1; then
			docker rm -f "${name}" >/dev/null 2>&1 || echo "warning: failed to remove ${name}" >&2
		fi
	done
	local ref
	for ref in "${image_ref}" "${unheld_ref}" "${base_ref}" "${substitution_ref}" \
		"${cold_ref}" "${share_ref}" "${share_more_ref}" "${partial_ref}"; do
		if [ -n "${ref}" ] && docker image inspect "${ref}" >/dev/null 2>&1; then
			docker rmi -f "${ref}" >/dev/null 2>&1 || echo "warning: failed to remove ${ref}" >&2
		fi
	done
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
	# Either phase's failure leaves its own daemon running; put the
	# devcontainer back in its boot posture (staged mode, default stores),
	# pinned against any mode or store the caller's environment carries.
	# On the success path this reconciles to a no-op.
	CFS_ALLOW_UNSTAGED_PULLS=1 CFS_ROOT="${WARM_CFS_ROOT}" KNARR_ROOT="${WARM_KNARR_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		echo "warning: failed to restore the boot snapshotter posture" >&2
}

fail() {
	echo "E2E FAIL: $1" >&2
	tail -80 "${CFS_LOG}" >&2 2>/dev/null || echo "warning: failed to read ${CFS_LOG}" >&2
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		snapshots --snapshotter cfs ls >&2 2>/dev/null || echo "warning: failed to list cfs snapshots" >&2
	exit 1
}

# wait_until polls a predicate, failing the test when it never holds.
wait_until() {
	local what="$1" tries="$2" delay="$3"
	shift 3
	local _
	for _ in $(seq 1 "${tries}"); do
		if "$@"; then
			return 0
		fi
		sleep "${delay}"
	done
	fail "timed out waiting for ${what}"
}

knarr_cli() {
	"${KNARR_BIN}" --root "${knarr_root}" "$@"
}

content_has() {
	local digest="$1" listing
	listing="$(ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" content ls -q)" ||
		fail "cannot list containerd content"
	grep -Fxq "${digest}" <<<"${listing}"
}

layers_absent() {
	local manifest="$1" digest
	while read -r digest; do
		if content_has "${digest}"; then
			return 1
		fi
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	return 0
}

cfs_snapshot_keys() {
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		snapshots --snapshotter cfs ls | awk 'NR > 1 {print $1}' | sort
}

# cfs_committed_keys lists the committed snapshot records, sorted. The kind is
# the last column: a record with no parent leaves that column empty.
cfs_committed_keys() {
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		snapshots --snapshotter cfs ls | awk 'NR > 1 && $NF == "Committed" {print $1}' | sort
}

# The polling predicates below capture their snapshot listing first and fail
# the test outright when the listing itself cannot be taken: a broken query
# must not read as an empty or changed snapshot set.
no_cfs_snapshots() {
	local keys
	keys="$(cfs_snapshot_keys)" || fail "cannot list cfs snapshots"
	[ -z "${keys}" ]
}

no_active_snapshots() {
	local listing actives
	listing="$(ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		snapshots --snapshotter cfs ls)" || fail "cannot list cfs snapshots"
	actives="$(awk 'NR > 1 && $NF == "Active" {n++} END {print n + 0}' <<<"${listing}")"
	[ "${actives}" -eq 0 ]
}

committed_keys_differ_from() {
	local keys
	keys="$(cfs_committed_keys)" || fail "cannot list cfs snapshots"
	[ "${keys}" != "$1" ]
}

committed_keys_are() {
	local keys
	keys="$(cfs_committed_keys)" || fail "cannot list cfs snapshots"
	[ "${keys}" = "$1" ]
}

# collected_after_lease_drop re-drops any lease taken since the pull began and
# then checks whether the staged records are gone. The runtime creates its
# hours-long retention lease while it unwinds the cancelled pull, so a single
# drop can run before that lease exists; dropping on every poll tick closes
# the race. Each tick also creates and deletes a scratch lease of its own:
# collection only happens when the garbage collector runs, the collector is
# armed by deletion events, and a cleanly unwound pull leaves nothing to
# delete — on a quiet runtime the unreferenced records would then just sit.
collected_after_lease_drop() {
	drop_new_leases "$1"
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		leases create --id cfs-e2e-gc-kick >/dev/null || fail "cannot create the gc-kick lease"
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
		leases rm cfs-e2e-gc-kick >/dev/null || fail "cannot remove the gc-kick lease"
	committed_keys_are "$2"
}

lease_ids() {
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" leases ls -q | sort
}

# drop_new_leases releases the leases taken since the given listing. An
# interrupted pull leaves its own behind, and the runtime lets it expire hours
# later rather than deleting it on the way out; dropping it is what puts the
# snapshots it referenced in reach of the garbage collector now.
drop_new_leases() {
	local before="$1" id
	while IFS= read -r id; do
		[ -z "${id}" ] && continue
		ctr --address "${CONTAINERD_SOCKET}" --namespace "${DOCKER_NAMESPACE}" \
			leases rm "${id}" >/dev/null || fail "cannot release lease ${id}"
	done < <(comm -13 <(printf '%s\n' "${before}") <(lease_ids))
}

chain_mount() {
	awk -v root="${cfs_root}/snapshots/" \
		'index($5, root) == 1 && $5 ~ /\/[0-9]+\/fs$/ {print $5}' \
		/proc/self/mountinfo | tail -1
}

chain_mounts() {
	awk -v root="${cfs_root}/snapshots/" \
		'index($5, root) == 1 && $5 ~ /\/[0-9]+\/fs$/ {n++} END {print n + 0}' \
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

# cas_dir is the content-addressed object store: one file per stored object,
# named by its content digest. verity_view_dir is the tree of hardlinks to
# those same objects named by their measured fs-verity digest — the names a
# merged image's redirects resolve through, so it is what a container reads.
cas_dir() {
	printf '%s/objects/sha256\n' "${knarr_root}"
}

verity_view_dir() {
	printf '%s/objects/fsverity-sha256-12\n' "${knarr_root}"
}

sharded_path() {
	local dir="$1" hex="${2#sha256:}"
	printf '%s/%s/%s\n' "${dir}" "${hex:0:2}" "${hex:2}"
}

cas_object_bytes() {
	find "$(cas_dir)" -type f -printf '%s\n' | awk '{n += $1} END {print n + 0}'
}

# image_bindings counts the composefs images the store has built for a root
# digest — one per platform, so one on a single-platform device.
image_bindings() {
	local hex="${1#sha256:}" dir
	dir="${knarr_root}/images/sha256/${hex:0:2}/${hex:2}"
	[ -d "${dir}" ] || fail "the knarr store has no composefs binding for $1"
	find "${dir}" -type f -printf '.\n' | wc -l
}

# staging_uppers counts the upper directories of staged layers still on disk.
# A converted chain has none: its content lives in the knarr store.
staging_uppers() {
	find "${cfs_root}/snapshots" -mindepth 2 -maxdepth 2 -name upper -printf '.\n' | wc -l
}

# rm_residue counts the directories a removal renamed out of the way and has
# not yet deleted.
rm_residue() {
	find "${cfs_root}/snapshots" -mindepth 1 -maxdepth 1 -name 'rm-*' -printf '.\n' | wc -l
}

no_staging_residue() {
	[ "$(staging_uppers)" -eq 0 ] && [ "$(rm_residue)" -eq 0 ]
}

# assert_no_staging_residue waits for the snapshot directories to be gone.
# containerd drops its own record first and deletes the snapshot behind it
# afterwards, so an empty listing runs slightly ahead of an empty disk.
assert_no_staging_residue() {
	local uppers residue
	for _ in $(seq 1 30); do
		if no_staging_residue; then
			return 0
		fi
		sleep 1
	done
	uppers="$(staging_uppers)"
	residue="$(rm_residue)"
	fail "${uppers} staged upper and ${residue} half-removed directories were left behind"
}

# drain_runtime_images removes every image the runtime holds and waits for
# containerd to forget the cfs snapshots that went with them, so no record
# outlives the store it was made in.
drain_runtime_images() {
	local images
	images="$(docker image ls -aq | sort -u)" || fail "cannot list runtime images"
	if [ -n "${images}" ]; then
		xargs -r -n1 docker rmi -f <<<"${images}" >/dev/null ||
			fail "cannot remove the runtime images"
	fi
	wait_until "containerd to forget the cfs snapshots" 60 1 no_cfs_snapshots
}

forget_knarr_images() {
	local listing refs ref
	listing="$(knarr_cli ls --format json)" || fail "cannot list staged knarr images"
	refs="$(jq -r '.[].ref' <<<"${listing}")" || fail "cannot parse staged knarr images"
	while IFS= read -r ref; do
		[ -z "${ref}" ] && continue
		knarr_cli remove "${ref}" >&2 || fail "cannot forget staged knarr image ${ref}"
	done <<<"${refs}"
}

# knarr_ref_for_digest reports the reference the store recorded for a root
# digest. A pull records the reference it was invoked with, which is docker's
# for a converted image and the staging command's for one put there out of band.
knarr_ref_for_digest() {
	local digest="$1" listing ref
	listing="$(knarr_cli ls --format json)" || fail "cannot list staged knarr images"
	ref="$(jq -r --arg d "${digest}" 'map(select(.digest == $d)) | .[0].ref // ""' <<<"${listing}")" ||
		fail "cannot parse staged knarr images"
	[ -n "${ref}" ] || fail "the knarr store holds no image for ${digest}"
	printf '%s\n' "${ref}"
}

assert_knarr_remove_declines() {
	local ref="$1" output
	if output="$(knarr_cli remove "${ref}" 2>&1)"; then
		fail "knarr removed ${ref} while the runtime was still holding it"
	fi
	grep -Fq 'is held by cfs' <<<"${output}" ||
		fail "knarr remove ${ref} did not decline on the cfs hold: ${output}"
}

# knarr_remove_when_released removes an image once the snapshotter has let go
# of it. The release rides containerd's garbage collector, so it lands some
# time after the runtime forgets the image; any other failure is fatal at once.
knarr_remove_when_released() {
	local ref="$1" output=""
	for _ in $(seq 1 60); do
		if output="$(knarr_cli remove "${ref}" 2>&1)"; then
			return 0
		fi
		grep -Fq 'is held by cfs' <<<"${output}" ||
			fail "knarr remove ${ref} failed for another reason: ${output}"
		sleep 1
	done
	fail "the cfs hold on ${ref} was never released: ${output}"
}

seed_image() {
	local nonce="$1" ref="$2" layer_count="$3" layout="$4"
	local alternate_config="${5:-}" layer_bytes="${6:-}" index_root="${7:-}"
	SEED_LAYER_COUNT="${layer_count}" SEED_NONCE="${nonce}" SEED_RUNNABLE=1 \
		SEED_ALTERNATE_CONFIG="${alternate_config}" SEED_LAYER_BYTES="${layer_bytes}" \
		SEED_INDEX_ROOT="${index_root}" \
		SEED_KEEP_LAYOUT="${layout}" "${SCRIPT_DIR}/seed-registry.sh" "${ref}" >&2 ||
		fail "cannot seed ${ref}"
}

manifest_digest() {
	jq -r '.manifests[0].digest' "${1}/index.json"
}

# indexed_manifest_digest reports the platform manifest an index-rooted
# layout's tag resolves to: the sole entry of the index blob the layout's
# top-level index points at.
indexed_manifest_digest() {
	local layout="$1" idx
	idx="$(manifest_digest "${layout}")" || return 1
	jq -r '.manifests[0].digest' "${layout}/blobs/sha256/${idx#sha256:}"
}

# ref_record_field reads one field of the record the knarr store keeps for a
# reference, straight from the on-disk refs tree.
ref_record_field() {
	local ref="$1" field="$2" value
	value="$(jq -r --arg r "${ref}" "select(.ref == \$r) | ${field}" \
		"${knarr_root}"/refs/sha256/*/*.json 2>/dev/null)" ||
		fail "cannot read the ref records under ${knarr_root}"
	[ -n "${value}" ] || fail "the knarr store has no ref record for ${ref}"
	printf '%s\n' "${value}"
}

# no_image_binding reports whether the store holds no composefs binding for a
# root digest — the shape image_bindings treats as a test failure.
no_image_binding() {
	local hex="${1#sha256:}"
	[ ! -d "${knarr_root}/images/sha256/${hex:0:2}/${hex:2}" ]
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
	knarr_cli pull --from-dir "${layout}" "${repo}@${digest}" >&2 ||
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

assert_warm_hold_declines_remove() {
	local nonce="$1" ref output
	ref="$(knarr_ref_for_digest "$(manifest_digest "${work}/layout")")"
	assert_knarr_remove_declines "${ref}"
	output="$(docker run --rm --network=none "${image_ref}")" ||
		fail "docker run after the declined removal failed"
	[ "${output}" = "cfs hello ${nonce}" ] || fail "unexpected container output: ${output}"
	echo "PASS: an admitted image is cfs-held and knarr declined to remove it"
}

assert_warm_hold_released() {
	local ref
	ref="$(knarr_ref_for_digest "$(manifest_digest "${work}/layout")")"
	knarr_remove_when_released "${ref}"
	echo "PASS: removing the image from the runtime released the cfs hold"
}

# assert_warm_tag_identity stages an index-rooted image by TAG through the
# registry door and lets the runtime pull the same tag: the tag resolves to
# an index digest, but the image is keyed by the platform manifest the index
# selects, so admission, the zero-fetch answer, the hold, and the removal
# all meet on that one identity.
assert_warm_tag_identity() {
	local nonce="$1" repo="cfs/e2e-tag" layout="${work}/tag-layout"
	local tag_ref idx mdigest manifest config digest output
	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-tag" "${tag_ref}" 2 "${layout}" "" "" 1
	idx="$(manifest_digest "${layout}")" || fail "cannot read the index digest"
	mdigest="$(indexed_manifest_digest "${layout}")" || fail "cannot read the platform manifest digest"
	[ "${idx}" != "${mdigest}" ] || fail "the seeded tag does not resolve through an index"

	knarr_cli pull "${tag_ref}" >&2 || fail "knarr pull by tag failed"
	[ "$(knarr_cli inspect --format json "${tag_ref}" | jq -r '.root')" = "${mdigest}" ] ||
		fail "the tag pull is not keyed by the platform manifest"
	[ "$(ref_record_field "${tag_ref}" '.resolved.digest')" = "${idx}" ] ||
		fail "the ref record does not keep the index as provenance"
	no_image_binding "${idx}" || fail "the store built a binding under the index digest"

	manifest="$(cat "${layout}/blobs/sha256/${mdigest#sha256:}")"
	config="$(jq -r '.config.digest' <<<"${manifest}")"
	docker pull "${tag_ref}" >/dev/null || fail "docker pull of the staged tag failed"
	while read -r digest; do
		content_has "${digest}" && fail "layer was fetched: ${digest}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	content_has "${config}" || fail "image config was not fetched"
	output="$(docker run --rm --network=none "${tag_ref}")" || fail "docker run of the tag failed"
	[ "${output}" = "cfs hello ${nonce}-tag" ] || fail "unexpected container output: ${output}"

	assert_knarr_remove_declines "${tag_ref}"
	docker rmi "${tag_ref}" >/dev/null || fail "docker rmi of the tag failed"
	knarr_remove_when_released "${tag_ref}"
	echo "PASS: a tag staged out of band admitted, held, and removed by its manifest identity"
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
	CFS_ALLOW_UNSTAGED_PULLS=0 CFS_ROOT="${WARM_CFS_ROOT}" KNARR_ROOT="${WARM_KNARR_ROOT}" \
		CFS_FORCE_RESTART=1 "${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		fail "restart snapshotter"
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

# hello_objects lists the names under dir holding the seeded /hello binary of
# the image in layout. Its content is the same in every image seeded with the
# same nonce, so a store that deduplicates keeps exactly one.
hello_objects() {
	local dir="$1" layout="$2" mdigest layer candidate
	mdigest="$(manifest_digest "${layout}")" || fail "cannot read manifest digest"
	layer="$(jq -r '.layers[0].digest' "${layout}/blobs/sha256/${mdigest#sha256:}")"
	tar -xzOf "${layout}/blobs/sha256/${layer#sha256:}" ./hello >"${work}/hello" ||
		fail "extract seeded /hello"
	while IFS= read -r candidate; do
		if cmp -s "${candidate}" "${work}/hello"; then
			printf '%s\n' "${candidate}"
		fi
	done < <(find "${dir}" -type f -size "$(stat -c%s "${work}/hello")c")
}

assert_tamper_fails_at_open() {
	local object output tamper_chain
	object="$(hello_objects "$(verity_view_dir)" "${work}/layout" | sed -n '1p')"
	[ -n "${object}" ] || fail "cannot find /hello in the knarr object view"
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

# report_conversion prints how long building the staged layers into a composefs
# image took. The snapshotter traces every Commit as it starts, and the whole
# chain is built inside the last one; the caller asks for the record that
# commit created as soon as the call returns, so the gap between the two is the
# build. Everything else the daemon logs meanwhile belongs to other records.
report_conversion() {
	local ref="$1" layers="$2" offset="$3" entries commit start name end seconds
	entries="$(tail -n "+$((offset + 1))" "${CFS_LOG}" | grep '^{')" ||
		fail "the snapshotter logged nothing for the pull of ${ref}"
	commit="$(jq -c 'select(.msg == "cfs snapshotter: commit")' <<<"${entries}" | tail -1)"
	[ -n "${commit}" ] || fail "the snapshotter logged no commit for the pull of ${ref}"
	start="$(jq -r '.time' <<<"${commit}")"
	name="$(jq -r '.name' <<<"${commit}")"
	end="$(jq -r --arg t "${start}" --arg k "${name}" \
		'select(.time > $t and .msg == "cfs snapshotter: stat" and .key == $k) | .time' \
		<<<"${entries}" | sed -n '1p')"
	[ -n "${end}" ] || fail "the snapshotter never served the record it committed for ${ref}"
	seconds="$(awk -v a="$(date -d "${start}" +%s.%N)" -v b="$(date -d "${end}" +%s.%N)" \
		'BEGIN {printf "%.3f", b - a}')"
	echo "conversion: ${ref} ${layers} layers in ${seconds}s"
}

assert_cold_pull_lands_composefs() {
	local nonce="$1" repo="cfs/e2e-cold" layout="${work}/cold-layout"
	local tag_ref offset info root composefs fsverity object view
	local bindings uppers records output pid chainfs

	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-cold" "${tag_ref}" 3 "${layout}"
	cold_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the cold image ref"

	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${cold_ref}" >/dev/null || fail "cold docker pull failed"
	report_conversion "${cold_ref}" 3 "${offset}"

	info="$(knarr_cli inspect --format json "${cold_ref}")" ||
		fail "the knarr store does not hold the cold-pulled image"
	root="$(manifest_digest "${layout}")"
	[ "$(jq -r '.root' <<<"${info}")" = "${root}" ] ||
		fail "the knarr store holds the cold-pulled image under another root"

	composefs="$(jq -r '.composefs.digest' <<<"${info}")"
	fsverity="$(jq -r '.fsverity' <<<"${info}")"
	object="$(sharded_path "$(cas_dir)" "${composefs}")"
	[ -f "${object}" ] || fail "the composefs image is missing from the store at ${object}"
	view="$(sharded_path "$(verity_view_dir)" "${fsverity}")"
	[ -f "${view}" ] || fail "the composefs image has no fs-verity view entry at ${view}"

	bindings="$(image_bindings "${root}")"
	[ "${bindings}" -eq 1 ] || fail "the cold pull built ${bindings} composefs images, want 1"

	uppers="$(staging_uppers)"
	[ "${uppers}" -eq 0 ] || fail "${uppers} staged upper directories survived the conversion"
	records="$(cfs_committed_keys | wc -l)"
	[ "${records}" -eq 3 ] || fail "the cold pull left ${records} snapshot records, want 3"

	output="$(docker run --rm --network=none "${cold_ref}")" ||
		fail "docker run of the cold-pulled image failed"
	[ "${output}" = "cfs hello ${nonce}-cold" ] || fail "unexpected container output: ${output}"

	docker run -d --network=none --name cfs-e2e-cold "${cold_ref}" /hello sleep >/dev/null ||
		fail "start the cold-pulled image for mount inspection"
	pid="$(docker inspect -f '{{.State.Pid}}' cfs-e2e-cold)"
	findmnt -N "${pid}" -n -o OPTIONS --target / | grep -q 'verity=require' ||
		fail "the cold-pulled container rootfs does not require fs-verity"
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "the cold-pulled image has no shared image mount"
	[ "$(findmnt -n -o FSTYPE -M "${chainfs}")" = erofs ] || fail "the cold image mount is not EROFS"
	docker rm -f cfs-e2e-cold >/dev/null || fail "remove the mount inspection container"
	echo "PASS: a plain docker pull staged, converted, and ran one composefs image"
}

assert_cold_repull_refetches() {
	local nonce="$1" layout="${work}/cold-layout" mdigest manifest digest output
	mdigest="$(manifest_digest "${layout}")" || fail "cannot read manifest digest"
	manifest="$(cat "${layout}/blobs/sha256/${mdigest#sha256:}")"

	docker rmi "${cold_ref}" >/dev/null || fail "docker rmi of the converted image failed"
	wait_until "containerd to drop the converted image's layers" 60 1 layers_absent "${manifest}"
	# A staging device refetches, never answers a pull from the store: the
	# re-pull must land every layer back in containerd's content store, so a
	# later pull extending this image always finds its shared layers there.
	docker pull "${cold_ref}" >/dev/null || fail "re-pull of the converted image failed"
	while read -r digest; do
		content_has "${digest}" || fail "the re-pull did not fetch layer ${digest}"
	done < <(jq -r '.layers[].digest' <<<"${manifest}")
	[ "$(image_bindings "${mdigest}")" -eq 1 ] ||
		fail "the re-pull did not converge on exactly one composefs image"
	output="$(docker run --rm --network=none "${cold_ref}")" ||
		fail "docker run after the re-pull failed"
	[ "${output}" = "cfs hello ${nonce}-cold" ] || fail "unexpected container output: ${output}"
	echo "PASS: re-pulling a converted image refetched its layers and kept one image"
}

assert_cold_prefix_dedup() {
	local nonce="$1" share_nonce="${nonce}-share"
	local repo="cfs/e2e-share" more_repo="cfs/e2e-share-more"
	local layout="${work}/share-layout" more_layout="${work}/share-more-layout"
	local before_bytes after_bytes copies

	# Both images are seeded from the same nonce, the second one layer longer,
	# so the first three layers are byte-identical. The shorter one is pulled,
	# removed from the runtime, and pulled again before the longer one: the
	# longer one's unpack then skips the shared chains — their records exist
	# from the re-pull — and its conversion reads every shared layer out of
	# the content store, which only works because the re-pull refetched them
	# rather than being answered from the knarr store.
	seed_image "${share_nonce}" "${REGISTRY}/${repo}:${nonce}" 3 "${layout}"
	share_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the shared-base ref"
	docker pull "${share_ref}" >/dev/null || fail "cold docker pull of the shared-base image failed"
	knarr_cli inspect --format json "${share_ref}" >/dev/null ||
		fail "the knarr store does not hold the shared-base image"
	docker rmi "${share_ref}" >/dev/null || fail "docker rmi of the shared-base image failed"
	local share_manifest
	share_manifest="$(cat "${layout}/blobs/sha256/$(manifest_digest "${layout}" | sed s/sha256://)")"
	wait_until "containerd to drop the shared-base layers" 60 1 layers_absent "${share_manifest}"
	docker pull "${share_ref}" >/dev/null || fail "re-pull of the shared-base image failed"

	before_bytes="$(cas_object_bytes)"
	seed_image "${share_nonce}" "${REGISTRY}/${more_repo}:${nonce}" 4 "${more_layout}"
	share_more_ref="$(pinned_ref "${REGISTRY}/${more_repo}" "${more_layout}")" ||
		fail "cannot pin the prefix-sharing ref"
	docker pull "${share_more_ref}" >/dev/null ||
		fail "cold docker pull of the prefix-sharing image failed"
	knarr_cli inspect --format json "${share_more_ref}" >/dev/null ||
		fail "the knarr store does not hold the prefix-sharing image"
	[ "$(image_bindings "$(manifest_digest "${more_layout}")")" -eq 1 ] ||
		fail "the prefix-sharing pull did not build exactly one composefs image"

	after_bytes="$(cas_object_bytes)"
	[ "${after_bytes}" -lt "$((before_bytes * 2))" ] ||
		fail "the store grew from ${before_bytes} to ${after_bytes} bytes; the shared layers were stored twice"
	copies="$(hello_objects "$(cas_dir)" "${more_layout}" | wc -l)"
	[ "${copies}" -eq 1 ] || fail "the store holds ${copies} copies of the shared /hello, want 1"
	echo "PASS: a prefix-sharing image cold-pulled after a base re-pull and shared its objects"
}

assert_cold_hold_released() {
	local ref
	ref="$(knarr_ref_for_digest "$(manifest_digest "${work}/cold-layout")")"
	assert_knarr_remove_declines "${ref}"
	docker rmi "${cold_ref}" >/dev/null || fail "docker rmi of the converted image failed"
	knarr_remove_when_released "${ref}"
	echo "PASS: a converted image stayed cfs-held until the runtime forgot it"
}

assert_interrupted_pull_converges() {
	local nonce="$1" repo="cfs/e2e-cold-partial" layout="${work}/partial-layout"
	local tag_ref baseline leases offset

	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-partial" "${tag_ref}" 3 "${layout}" "" "${PARTIAL_LAYER_BYTES}"
	partial_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" ||
		fail "cannot pin the interrupted image ref"

	baseline="$(cfs_committed_keys)"
	leases="$(lease_ids)"
	docker pull "${partial_ref}" >/dev/null 2>&1 &
	pull_pid=$!
	# The client going away is what cancels the pull, and the pull has to have
	# staged something first: poll until a layer is committed, which happens
	# while the last, large layer is still being transferred.
	wait_until "a staged layer of ${partial_ref} to be committed" 600 0.1 \
		committed_keys_differ_from "${baseline}"
	kill "${pull_pid}" || fail "cannot interrupt the cold pull"
	if wait "${pull_pid}"; then
		fail "the interrupted docker pull reported success"
	fi
	pull_pid=""

	wait_until "the aborted pull's active snapshot to go" 60 1 no_active_snapshots
	if docker image inspect "${partial_ref}" >/dev/null 2>&1; then
		fail "the cold pull completed before it could be interrupted"
	fi
	wait_until "the aborted pull's staged snapshots to be collected" 120 1 \
		collected_after_lease_drop "${leases}" "${baseline}"
	assert_no_staging_residue

	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${partial_ref}" >/dev/null || fail "re-pull after the interrupted pull failed"
	report_conversion "${partial_ref}" 3 "${offset}"
	knarr_cli inspect --format json "${partial_ref}" >/dev/null ||
		fail "the re-pull did not land in the knarr store"
	[ "$(image_bindings "$(manifest_digest "${layout}")")" -eq 1 ] ||
		fail "the re-pull did not build exactly one composefs image"
	assert_no_staging_residue
	echo "PASS: an interrupted cold pull left nothing behind and a re-pull converged"
}

# assert_cold_mixed_door covers the two doors meeting on one image: the
# runtime stages a tag pull and holds the platform manifest it stamped, then
# a knarr pull of the same tag resolves its index down to that same manifest
# — updating the reference's provenance, not adding a second image — and a
# knarr remove of the tag declines on the runtime's hold instead of
# orphaning it.
assert_cold_mixed_door() {
	local nonce="$1" repo="cfs/e2e-mixed" layout="${work}/mixed-layout"
	local tag_ref idx mdigest resolved output
	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-mixed" "${tag_ref}" 2 "${layout}" "" "" 1
	idx="$(manifest_digest "${layout}")" || fail "cannot read the index digest"
	mdigest="$(indexed_manifest_digest "${layout}")" || fail "cannot read the platform manifest digest"
	[ "${idx}" != "${mdigest}" ] || fail "the seeded tag does not resolve through an index"

	docker pull "${tag_ref}" >/dev/null || fail "docker pull of the index-rooted tag failed"
	[ "$(image_bindings "${mdigest}")" -eq 1 ] ||
		fail "the staged pull did not build exactly one composefs image"
	resolved="$(ref_record_field "${tag_ref}" '.resolved.digest')"
	[ "${resolved}" = "${mdigest}" ] ||
		fail "the staged record's provenance is ${resolved}, want the stamped manifest"

	knarr_cli pull "${tag_ref}" >&2 || fail "knarr pull of the staged tag failed"
	[ "$(image_bindings "${mdigest}")" -eq 1 ] ||
		fail "the knarr pull built a second composefs image"
	no_image_binding "${idx}" || fail "the knarr pull built a binding under the index digest"
	[ "$(ref_record_field "${tag_ref}" '.identity.digest')" = "${mdigest}" ] ||
		fail "the knarr pull re-keyed the image away from the manifest identity"
	[ "$(ref_record_field "${tag_ref}" '.resolved.digest')" = "${idx}" ] ||
		fail "the knarr pull did not record the index as provenance"

	assert_knarr_remove_declines "${tag_ref}"
	output="$(docker run --rm --network=none "${tag_ref}")" ||
		fail "docker run after the declined removal failed"
	[ "${output}" = "cfs hello ${nonce}-mixed" ] || fail "unexpected container output: ${output}"

	docker rmi "${tag_ref}" >/dev/null || fail "docker rmi of the mixed-door image failed"
	knarr_remove_when_released "${tag_ref}"
	echo "PASS: a knarr pull of a runtime-staged tag kept one held image and remove declined"
}

warm_phase() {
	local nonce="$1" chainfs
	echo "== warm phase: images enter the store out of band =="
	drain_runtime_images
	# The devcontainer boots in staged mode; the warm legs need the
	# out-of-band posture, pinned explicitly rather than inherited.
	CFS_ALLOW_UNSTAGED_PULLS=0 CFS_ROOT="${WARM_CFS_ROOT}" KNARR_ROOT="${WARM_KNARR_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		fail "start the snapshotter in out-of-band mode"
	forget_knarr_images
	assert_handshake_identity "${nonce}"
	seed_and_stage "${nonce}"
	assert_real_zero_fetch_run "${nonce}"
	assert_warm_hold_declines_remove "${nonce}"
	chainfs="$(chain_mount)"
	[ -n "${chainfs}" ] || fail "no shared image mount found"
	assert_concurrent_shared_mount "${chainfs}"
	assert_mount_recovery "${nonce}" "${chainfs}"
	assert_gc_teardown "${chainfs}"
	assert_tamper_fails_at_open
	assert_warm_hold_released
	assert_warm_tag_identity "${nonce}"
}

cold_phase() {
	local nonce="$1"
	echo "== cold phase: the runtime's own pull fills the store =="
	# A device runs in one mode or the other, never both, so the cold phase
	# gets its own snapshotter and knarr stores; no record made in one mode is
	# ever read back in the other.
	drain_runtime_images
	# The roots flip before the daemon does: if the cold start dies halfway,
	# cleanup must already see the cold posture to restore the boot one.
	knarr_root="${COLD_KNARR_ROOT}"
	cfs_root="${COLD_CFS_ROOT}"
	CFS_ALLOW_UNSTAGED_PULLS=1 CFS_ROOT="${COLD_CFS_ROOT}" KNARR_ROOT="${COLD_KNARR_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "start the snapshotter in cold-pull mode"
	forget_knarr_images

	assert_cold_pull_lands_composefs "${nonce}"
	assert_cold_repull_refetches "${nonce}"
	assert_cold_hold_released
	assert_cold_prefix_dedup "${nonce}"
	assert_interrupted_pull_converges "${nonce}"
	assert_cold_mixed_door "${nonce}"

	drain_runtime_images
	CFS_ALLOW_UNSTAGED_PULLS=1 CFS_ROOT="${WARM_CFS_ROOT}" KNARR_ROOT="${WARM_KNARR_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "restore the boot snapshotter posture"
	knarr_root="${WARM_KNARR_ROOT}"
	cfs_root="${WARM_CFS_ROOT}"
}

main() {
	[ -x "${KNARR_BIN}" ] ||
		fail "knarr CLI not found at ${KNARR_BIN}; run the e2e from the host as
./devshell.sh make test-e2e — devshell builds the knarr CLI there and
mounts it into the container (a container started any other way has none)"
	work="$(mktemp -d)"
	trap cleanup EXIT
	local nonce="${RANDOM}${RANDOM}"
	warm_phase "${nonce}"
	cold_phase "${nonce}"
	echo "E2E PASS: handshake identity, real content, recovery, concurrency, teardown," \
		"tamper enforcement, holds, tag identity, cold conversion, dedup," \
		"interrupted-pull recovery, and mixed-door identity"
}

main "$@"
