#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CONTAINERD_SOCKET=/run/containerd/containerd.sock
DOCKER_NAMESPACE=moby
REGISTRY=127.0.0.1:5000
WARM_OCICFS_ROOT=/var/lib/ocicfs
WARM_CFS_ROOT=/var/lib/containerd-cfs-grpc
COLD_OCICFS_ROOT=/var/lib/ocicfs-cold
COLD_CFS_ROOT=/var/lib/containerd-cfs-grpc-cold
OCICFS_BIN=${OCICFS_BIN:-/opt/cfs-dev/bin/ocicfs}
CFS_LOG=/var/log/cfs-devcontainer/containerd-cfs-grpc.log
# Size of the last layer of the image the interrupt leg pulls. It is
# incompressible, so its transfer outlasts the poll waiting for the layers
# before it to be staged — the window the pull is interrupted in.
PARTIAL_LAYER_BYTES=134217728

work=""
# ocicfs_root and cfs_root name the stores the phase in progress runs against.
# The warm phase pins the preloaded mode on the devcontainer's default
# stores; the cold phase runs the admit-any mode — the boot posture — with
# stores of its own, because the two work modes are never mixed on one
# device.
ocicfs_root="${WARM_OCICFS_ROOT}"
cfs_root="${WARM_CFS_ROOT}"
image_ref=""
unheld_ref=""
base_ref=""
substitution_ref=""
cold_ref=""
share_ref=""
share_more_ref=""
# pair_base_ref and pair_derived_ref are the runtime's images of the
# base-after-derived leg while it runs, pair_agent_refs the references the
# leg's ocicfs pulls put in the store under pair_store; the leg clears them
# once it has removed them itself.
pair_base_ref=""
pair_derived_ref=""
pair_agent_refs=""
pair_store=""
partial_ref=""
delta_ref=""
# delta_base_ref is release N of the delta leg while the leg holds it as the
# delta base, so a failure inside the leg does not leave the hold behind.
delta_base_ref=""
pull_pid=""
# The load legs' archives, written by docker save while the registry is up,
# and the tags they carry; registry_down is set while those legs keep the
# registry stopped, so cleanup can start it again.
load_base_tar=""
load_derived_tar=""
load_base_tag=""
load_derived_tag=""
load_nonce=""
# load_pulled_ref is the digest reference the sideload-then-pull leg has the
# runtime pull, while the runtime may hold it.
load_pulled_ref=""
registry_down=""
# cold_composefs is the digest of the composefs image the cold pull built at
# one ingest worker, which the two-worker re-pull and an ocicfs pull of the same
# image must build again byte for byte.
cold_composefs=""
cold_store_bytes=""
fresh_ocicfs_root=""
# The run's progress, for the summary the exit trap prints: the section in
# progress, each check's section, status (ok or skip) and description, and the
# message and leg of a failure.
SECTION="setup"
CHECK_SECTIONS=()
CHECK_STATUS=()
CHECK_TITLES=()
FAILMSG=""
FAILLEG=""

cleanup() {
	local rc=$?
	# A failure inside the interrupted-pull leg can leave its background pull
	# running; it has to go before the images it references are removed.
	if [ -n "${pull_pid}" ] && kill -0 "${pull_pid}" 2>/dev/null; then
		kill "${pull_pid}" 2>/dev/null || echo "warning: failed to stop the background pull" >&2
		wait "${pull_pid}" 2>/dev/null || true
	fi
	if [ -n "${registry_down}" ]; then
		start_registry || echo "warning: failed to restart the registry" >&2
	fi
	local name
	for name in cfs-e2e-1 cfs-e2e-2 cfs-e2e-recovery cfs-e2e-cold cfs-e2e-delta cfs-e2e-pair; do
		if docker container inspect "${name}" >/dev/null 2>&1; then
			docker rm -f "${name}" >/dev/null 2>&1 || echo "warning: failed to remove ${name}" >&2
		fi
	done
	local ref
	for ref in "${image_ref}" "${unheld_ref}" "${base_ref}" "${substitution_ref}" \
		"${cold_ref}" "${share_ref}" "${share_more_ref}" "${partial_ref}" "${delta_ref}" \
		"${pair_base_ref}" "${pair_derived_ref}" "${load_base_tag}" "${load_derived_tag}" "${load_pulled_ref}"; do
		if [ -n "${ref}" ] && docker image inspect "${ref}" >/dev/null 2>&1; then
			docker rmi -f "${ref}" >/dev/null 2>&1 || echo "warning: failed to remove ${ref}" >&2
		fi
	done
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
	if [ -n "${fresh_ocicfs_root}" ] && [ -e "${fresh_ocicfs_root}" ]; then
		rm -rf "${fresh_ocicfs_root}" || echo "warning: failed to remove ${fresh_ocicfs_root}" >&2
	fi
	# The delta leg's hold and references live in the warm store; a failure
	# inside the leg must not leave them for the next run to trip over.
	if [ -n "${delta_base_ref}" ]; then
		"${OCICFS_BIN}" --root "${WARM_OCICFS_ROOT}" release --holder delta-base "${delta_base_ref}" >/dev/null 2>&1 ||
			echo "warning: failed to release the delta base ${delta_base_ref}" >&2
		for ref in "${delta_base_ref}" "${delta_ref}"; do
			[ -n "${ref}" ] || continue
			"${OCICFS_BIN}" --root "${WARM_OCICFS_ROOT}" inspect "${ref}" >/dev/null 2>&1 || continue
			"${OCICFS_BIN}" --root "${WARM_OCICFS_ROOT}" remove "${ref}" >/dev/null 2>&1 ||
				echo "warning: failed to remove ${ref} from the warm store" >&2
		done
		"${OCICFS_BIN}" --root "${WARM_OCICFS_ROOT}" gc >/dev/null 2>&1 ||
			echo "warning: failed to collect the warm store" >&2
	fi
	# The pair leg's agent references live in whichever store the leg ran
	# against; the runtime's holds on them go with the images removed above,
	# some time after, so the removal is retried before the store is collected.
	if [ -n "${pair_agent_refs}" ]; then
		local tries
		for ref in ${pair_agent_refs}; do
			for tries in $(seq 1 30); do
				"${OCICFS_BIN}" --root "${pair_store}" inspect "${ref}" >/dev/null 2>&1 || break
				"${OCICFS_BIN}" --root "${pair_store}" remove "${ref}" >/dev/null 2>&1 && break
				[ "${tries}" -lt 30 ] || echo "warning: failed to remove ${ref} from ${pair_store}" >&2
				sleep 1
			done
		done
		"${OCICFS_BIN}" --root "${pair_store}" gc >/dev/null 2>&1 ||
			echo "warning: failed to collect ${pair_store}" >&2
	fi
	# Either phase's failure leaves its own daemon running; put the
	# devcontainer back in its boot posture (admit-any mode, default stores),
	# pinned against any mode or store the caller's environment carries.
	# On the success path this reconciles to a no-op.
	CFS_ADMIT=any CFS_ROOT="${WARM_CFS_ROOT}" OCICFS_ROOT="${WARM_OCICFS_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		echo "warning: failed to restore the boot snapshotter posture" >&2
	summarize "${rc}"
}

# summarize prints the checks that ran, grouped by section, and a verdict as
# the last lines of the run, after any cleanup output.
summarize() {
	local rc="$1" i last="" passed=0 skipped=0
	printf '\nE2E summary (%ss):\n' "${SECONDS}"
	for i in "${!CHECK_TITLES[@]}"; do
		if [ "${CHECK_SECTIONS[i]}" != "${last}" ]; then
			last="${CHECK_SECTIONS[i]}"
			printf '  %s\n' "${last}"
		fi
		printf '    %-4s %2d. %s\n' "${CHECK_STATUS[i]}" $((i + 1)) "${CHECK_TITLES[i]}"
		case "${CHECK_STATUS[i]}" in
		ok) passed=$((passed + 1)) ;;
		skip) skipped=$((skipped + 1)) ;;
		esac
	done
	if [ "${rc}" -eq 0 ]; then
		echo "E2E OK: ${passed} passed, ${skipped} skipped"
		return
	fi
	# The failing leg never reached its check, so it gets a row of its own.
	[ "${SECTION}" = "${last}" ] || printf '  %s\n' "${SECTION}"
	printf '    %-4s %2d. %s\n' FAIL $((${#CHECK_TITLES[@]} + 1)) "${FAILLEG:-the step after the previous check}"
	echo "E2E FAIL in ${SECTION%%:*}${FAILLEG:+, ${FAILLEG}}: ${FAILMSG:-exit ${rc}}" >&2
}

# section starts a numbered part of the run; pass records a check that held
# and skip one that cannot run on this host.
section() {
	SECTION="$*"
	printf '\n=== %s ===\n' "${SECTION}"
}

pass() {
	CHECK_SECTIONS+=("${SECTION}")
	CHECK_STATUS+=(ok)
	CHECK_TITLES+=("$*")
	printf '  %-4s %2d. %s\n' ok "${#CHECK_TITLES[@]}" "$*"
}

skip() {
	CHECK_SECTIONS+=("${SECTION}")
	CHECK_STATUS+=(skip)
	CHECK_TITLES+=("$*")
	printf '  %-4s %2d. %s\n' skip "${#CHECK_TITLES[@]}" "$*"
}

# fail records the message and the leg (the outermost assert_* function) the
# exit trap reports, after dumping the snapshotter's state for diagnosis.
fail() {
	local fn
	FAILMSG="$1"
	for fn in "${FUNCNAME[@]}"; do
		case "${fn}" in assert_*) FAILLEG="${fn}" ;; esac
	done
	echo "FAIL: $1" >&2
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

ocicfs_cli() {
	"${OCICFS_BIN}" --root "${ocicfs_root}" "$@"
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
# active_gone_after_lease_drop releases the leases the aborted pull took — a
# retention lease the runtime leaves behind keeps its active record from
# going — and reports whether that record is gone.
active_gone_after_lease_drop() {
	drop_new_leases "$1"
	no_active_snapshots
}

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
	printf '%s/objects/sha256\n' "${ocicfs_root}"
}

verity_view_dir() {
	printf '%s/objects/fsverity-sha256-12\n' "${ocicfs_root}"
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
	dir="${ocicfs_root}/images/sha256/${hex:0:2}/${hex:2}"
	[ -d "${dir}" ] || fail "the ocicfs store has no composefs binding for $1"
	find "${dir}" -type f -printf '.\n' | wc -l
}

# staging_uppers counts the upper directories of staged layers still on disk.
# A converted chain has none: its content lives in the ocicfs store.
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

forget_ocicfs_images() {
	local listing refs ref
	listing="$(ocicfs_cli ls --format json)" || fail "cannot list staged ocicfs images"
	refs="$(jq -r '.[].ref' <<<"${listing}")" || fail "cannot parse staged ocicfs images"
	while IFS= read -r ref; do
		[ -z "${ref}" ] && continue
		ocicfs_cli remove "${ref}" >&2 || fail "cannot forget staged ocicfs image ${ref}"
	done <<<"${refs}"
}

# ocicfs_ref_for_digest reports the reference the store recorded for a root
# digest. A pull records the reference it was invoked with, which is docker's
# for a converted image and the staging command's for one put there out of band.
ocicfs_ref_for_digest() {
	local digest="$1" listing ref
	listing="$(ocicfs_cli ls --format json)" || fail "cannot list staged ocicfs images"
	ref="$(jq -r --arg d "${digest}" 'map(select(.digest == $d)) | .[0].ref // ""' <<<"${listing}")" ||
		fail "cannot parse staged ocicfs images"
	[ -n "${ref}" ] || fail "the ocicfs store holds no image for ${digest}"
	printf '%s\n' "${ref}"
}

# ocicfs_lists_digest succeeds when the store lists an image by its identity
# digest, whatever reference it is listed under — none, for an image the
# runtime staged, which the store keeps by the snapshotter's hold alone.
ocicfs_lists_digest() {
	local digest="$1" listing
	listing="$(ocicfs_cli ls --format json)" || fail "cannot list ocicfs images"
	jq -e --arg d "${digest}" 'any(.[]; .digest == $d)' <<<"${listing}" >/dev/null
}

# ocicfs_ref_of_digest prints the reference the store lists a digest under,
# empty for an image no reference names.
ocicfs_ref_of_digest() {
	local digest="$1" listing
	listing="$(ocicfs_cli ls --format json)" || fail "cannot list ocicfs images"
	jq -r --arg d "${digest}" 'map(select(.digest == $d)) | .[0].ref // ""' <<<"${listing}"
}

# ocicfs_holder_of_digest prints the holder a runtime reference names a digest
# for, empty for an agent's reference or an unnamed image.
ocicfs_holder_of_digest() {
	local digest="$1" listing
	listing="$(ocicfs_cli ls --format json)" || fail "cannot list ocicfs images"
	jq -r --arg d "${digest}" 'map(select(.digest == $d)) | .[0].holder // ""' <<<"${listing}"
}

# store_settled reports whether a collection would reclaim nothing: a build
# abandoned midway requests a collection, so what an interrupted pull wrote
# is gone shortly after its build is closed. While that collection runs the
# store is busy and the probe fails, which is "not yet", not an error.
store_settled() {
	local report
	report="$(ocicfs_cli gc --dry-run --format json 2>/dev/null)" || return 1
	[ "$(jq -r '.objectsRemoved' <<<"${report}")" -eq 0 ]
}

# collect_store runs one collection; a store busy with a pull or a mount
# makes it fail, which under wait_until is "not yet".
collect_store() {
	ocicfs_cli gc >/dev/null 2>&1
}

# shadow_object reports the store path of the shadow.txt a seeded layer
# writes, which every later layer of the image replaces.
shadow_object() {
	local nonce="$1" layer="$2" hex
	hex="$(for _ in $(seq 1 64); do printf 'cfs e2e shadow %s %s\n' "${layer}" "${nonce}"; done | sha256sum | cut -d' ' -f1)"
	sharded_path "$(cas_dir)" "${hex}"
}

assert_ocicfs_remove_declines() {
	local ref="$1" output
	if output="$(ocicfs_cli remove "${ref}" 2>&1)"; then
		fail "ocicfs removed ${ref} while the runtime was still holding it"
	fi
	grep -Fq 'is held by cfs' <<<"${output}" ||
		fail "ocicfs remove ${ref} did not decline on the cfs hold: ${output}"
}

# ocicfs_remove_when_released removes an image once the snapshotter has let go
# of it. The release rides containerd's garbage collector, so it lands some
# time after the runtime forgets the image; any other failure is fatal at once.
ocicfs_remove_when_released() {
	local ref="$1" output=""
	for _ in $(seq 1 60); do
		if output="$(ocicfs_cli remove "${ref}" 2>&1)"; then
			return 0
		fi
		grep -Fq 'is held by cfs' <<<"${output}" ||
			fail "ocicfs remove ${ref} failed for another reason: ${output}"
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

# ref_record_field reads one field of the record the ocicfs store keeps for a
# reference, straight from the on-disk refs tree.
ref_record_field() {
	local ref="$1" field="$2" value
	value="$(jq -r --arg r "${ref}" "select(.ref == \$r) | ${field}" \
		"${ocicfs_root}"/refs/sha256/*/*.json 2>/dev/null)" ||
		fail "cannot read the ref records under ${ocicfs_root}"
	[ -n "${value}" ] || fail "the ocicfs store has no ref record for ${ref}"
	printf '%s\n' "${value}"
}

# no_image_binding reports whether the store holds no composefs binding for a
# root digest — the shape image_bindings treats as a test failure.
no_image_binding() {
	local hex="${1#sha256:}"
	[ ! -d "${ocicfs_root}/images/sha256/${hex:0:2}/${hex:2}" ]
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
	ocicfs_cli pull --from-dir "${layout}" "${repo}@${digest}" >&2 ||
		fail "ocicfs pull failed for ${repo}"
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
	pass "an unheld config substitution with identical layers failed during docker pull"

	if output="$(docker pull "${unheld_ref}" 2>&1)"; then
		fail "docker pull succeeded for an image absent from ocicfs: ${output}"
	fi
	grep -Fq 'is not present in the local store' <<<"${output}" ||
		fail "unheld pull did not fail at the handshake identity gate: ${output}"
	pass "an unrelated image absent from ocicfs failed during docker pull"

	if output="$(docker pull "${base_ref}" 2>&1)"; then
		fail "docker pull succeeded for an unheld base prefix: ${output}"
	fi
	grep -Fq 'is not present in the local store' <<<"${output}" ||
		fail "base-prefix pull did not fail at the handshake identity gate: ${output}"
	pass "an unheld base prefix failed during docker pull"
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
	pass "ocicfs pull -> zero-fetch docker run served real image content"
}

assert_warm_hold_declines_remove() {
	local nonce="$1" ref output
	ref="$(ocicfs_ref_for_digest "$(manifest_digest "${work}/layout")")"
	assert_ocicfs_remove_declines "${ref}"
	output="$(docker run --rm --network=none "${image_ref}")" ||
		fail "docker run after the declined removal failed"
	[ "${output}" = "cfs hello ${nonce}" ] || fail "unexpected container output: ${output}"
	pass "an admitted image is cfs-held and ocicfs declined to remove it"
}

assert_warm_hold_released() {
	local ref
	# The runtime forgot the image; the agent's reference still names it, so
	# the store kept it and only the runtime's hold is gone.
	ref="$(ocicfs_ref_for_digest "$(manifest_digest "${work}/layout")")"
	ocicfs_remove_when_released "${ref}"
	pass "removing the image from the runtime released the cfs hold and kept the agent's image"
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

	ocicfs_cli pull "${tag_ref}" >&2 || fail "ocicfs pull by tag failed"
	[ "$(ocicfs_cli inspect --format json "${tag_ref}" | jq -r '.root')" = "${mdigest}" ] ||
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

	assert_ocicfs_remove_declines "${tag_ref}"
	docker rmi "${tag_ref}" >/dev/null || fail "docker rmi of the tag failed"
	ocicfs_remove_when_released "${tag_ref}"
	pass "a tag staged out of band admitted, held, and removed by its manifest identity"
}

# ocicfs_fixtures prints the directory of the pinned ocicfs module's
# zstd:chunked fixtures: two releases sharing a base layer, written by
# skopeo, as linux/amd64 single-image OCI layouts. The module lives in the
# module cache devshell hands the container.
ocicfs_fixtures() {
	local dir
	dir="$(cd "${ROOT_DIR}" && go list -m -f '{{.Dir}}' github.com/foundriesio/ocicfs)" ||
		fail "cannot locate the pinned ocicfs module in the module cache"
	dir="${dir}/internal/zstdchunked/testdata"
	[ -f "${dir}/release-n/index.json" ] && [ -f "${dir}/release-n1/index.json" ] ||
		fail "the pinned ocicfs module has no delta fixtures under ${dir}"
	printf '%s\n' "${dir}"
}

# push_layout publishes a single-image OCI layout to the registry under a
# tag, blob by blob over the distribution API, so the manifest lands byte
# for byte: the zstd:chunked annotations on its layer descriptors are what
# a delta pull reads. It checks the registry serves the manifest back under
# the digest the layout names it by.
push_layout() {
	local layout="$1" repo="$2" tag="$3" mdigest mtype blob digest location served
	mdigest="$(manifest_digest "${layout}")" || fail "cannot read the manifest digest of ${layout}"
	mtype="$(jq -r '.manifests[0].mediaType' "${layout}/index.json")" ||
		fail "cannot read the manifest media type of ${layout}"
	for blob in "${layout}"/blobs/sha256/*; do
		digest="sha256:${blob##*/}"
		[ "${digest}" != "${mdigest}" ] || continue
		location="$(curl -fsS -X POST -o /dev/null -D - "http://${REGISTRY}/v2/${repo}/blobs/uploads/" |
			awk 'tolower($1) == "location:" {print $2}' | tr -d '\r')" ||
			fail "cannot start the upload of ${digest}"
		[ -n "${location}" ] || fail "the registry offered no upload location for ${digest}"
		case "${location}" in
		http://* | https://*) ;;
		*) location="http://${REGISTRY}${location}" ;;
		esac
		case "${location}" in
		*\?*) location="${location}&digest=${digest}" ;;
		*) location="${location}?digest=${digest}" ;;
		esac
		curl -fsS -X PUT -H 'Content-Type: application/octet-stream' \
			--data-binary "@${blob}" -o /dev/null "${location}" ||
			fail "cannot push blob ${digest} of ${layout}"
	done
	curl -fsS -X PUT -H "Content-Type: ${mtype}" \
		--data-binary "@${layout}/blobs/sha256/${mdigest#sha256:}" -o /dev/null \
		"http://${REGISTRY}/v2/${repo}/manifests/${tag}" ||
		fail "cannot push the manifest of ${layout} as ${repo}:${tag}"
	served="$(curl -fsS -H "Accept: ${mtype}" "http://${REGISTRY}/v2/${repo}/manifests/${tag}" |
		sha256sum | cut -d' ' -f1)" || fail "cannot read back ${repo}:${tag}"
	[ "sha256:${served}" = "${mdigest}" ] ||
		fail "the registry serves ${repo}:${tag} as sha256:${served}, pushed ${mdigest}"
}

# assert_delta_rootfs reads release N+1's rootfs the way the runtime serves
# it — a container's export goes through the snapshotter's mounts — without
# running anything: the fixture carries no program, and create never runs
# the command it is given.
assert_delta_rootfs() {
	local tar="${work}/delta-rootfs.tar" listing
	docker create --network=none --name cfs-e2e-delta "${delta_ref}" /nonexistent >/dev/null ||
		fail "cannot create a container from the delta-pulled image"
	docker export -o "${tar}" cfs-e2e-delta || fail "cannot export the delta-pulled container's rootfs"
	docker rm cfs-e2e-delta >/dev/null || fail "cannot remove the exported container"
	[ "$(tar -xOf "${tar}" app/config)" = "v=2" ] ||
		fail "release N+1's app/config is not the delta-fetched content"
	listing="$(tar -tf "${tar}")" || fail "cannot list the exported rootfs"
	grep -Fxq app/new <<<"${listing}" || fail "release N+1's app/new is missing from the exported rootfs"
	! grep -Fxq app/old <<<"${listing}" || fail "release N's app/old is still in release N+1's rootfs"
}

# assert_delta_fast_path covers an image that entered the store by a delta
# pull. Release N is pulled and held as the delta base; release N+1 — the
# same base layer, an app layer that changes one file, drops one and adds
# one — is pulled by ranges over it and must be the image a plain pull
# builds; the runtime then gets it through the fast path like any staged
# image. Releasing, removing and collecting the base afterwards reclaims what
# only N carried and leaves N+1 whole.
assert_delta_fast_path() {
	local fixtures repo="cfs/e2e-release" n_layout n1_layout n_ref n1_digest n1_manifest
	local app_layer old_object log summary fetched size info plain_info config

	# The fixtures are linux/amd64 images; a host of another architecture
	# cannot pull them, so the leg is skipped there rather than failed.
	local arch
	arch="$(go env GOHOSTARCH)" || fail "cannot determine the host architecture"
	if [ "${arch}" != amd64 ]; then
		skip "delta fast path: the fixtures are linux/amd64 images"
		return 0
	fi
	fixtures="$(ocicfs_fixtures)"
	n_layout="${fixtures}/release-n"
	n1_layout="${fixtures}/release-n1"
	push_layout "${n_layout}" "${repo}" n
	push_layout "${n1_layout}" "${repo}" n1
	n_ref="$(pinned_ref "${REGISTRY}/${repo}" "${n_layout}")" || fail "cannot pin release N"
	delta_ref="$(pinned_ref "${REGISTRY}/${repo}" "${n1_layout}")" || fail "cannot pin release N+1"
	n1_digest="$(manifest_digest "${n1_layout}")"
	n1_manifest="$(cat "${n1_layout}/blobs/sha256/${n1_digest#sha256:}")"
	[ "$(jq -r '.layers[0].digest' <<<"${n1_manifest}")" = "$(image_layers "${n_layout}" | jq -r '.[0]')" ] ||
		fail "the two releases do not share their base layer"
	app_layer="$(jq -r '.layers[1].digest' <<<"${n1_manifest}")"
	# app/old is the one file only N carries; its object goes when N does.
	old_object="$(sharded_path "$(cas_dir)" "$(printf 'this file is bigger than sixty-four bytes so it is stored as an object.\nold\n' |
		sha256sum | cut -d' ' -f1)")"

	ocicfs_cli pull --delta "${n_ref}" >&2 || fail "delta pull of release N failed"
	[ -f "${old_object}" ] || fail "release N's app/old is not a store object after its pull"
	delta_base_ref="${n_ref}"
	ocicfs_cli hold --holder delta-base "${n_ref}" >&2 || fail "cannot hold release N as the delta base"

	log="${work}/delta.log"
	if ! ocicfs_cli pull --delta "${delta_ref}" >/dev/null 2>"${log}"; then
		cat "${log}" >&2
		fail "delta pull of release N+1 failed"
	fi
	cat "${log}" >&2
	[ "$(grep -c 'msg="delta: layer"' "${log}")" -eq 2 ] ||
		fail "the delta pull did not log one line per layer of release N+1"
	if grep 'msg="delta: layer"' "${log}" | grep -qv 'fallback=none'; then
		fail "a layer of release N+1 fell back to a whole fetch"
	fi
	grep -Eq "msg=\"delta: layer\" layer=${app_layer} .* requests=[1-9][0-9]* .* fallback=none" "${log}" ||
		fail "the app layer of release N+1 was not fetched by ranges"
	summary="$(grep 'msg="delta: pull"' "${log}")" || fail "the delta pull logged no summary line"
	fetched="$(sed -n 's/.* fetched=\([0-9]*\).*/\1/p' <<<"${summary}")"
	size="$(sed -n 's/.* size=\([0-9]*\).*/\1/p' <<<"${summary}")"
	[ "${fetched}" -gt 0 ] && [ "${fetched}" -lt "${size}" ] ||
		fail "the delta pull fetched ${fetched} of ${size} layer bytes"
	pass "release N+1 was pulled by ranges over the held release N"

	# The fresh store must sit on the shared fs-verity filesystem, like every
	# other store the devcontainer uses.
	fresh_ocicfs_root="$(mktemp -d /var/lib/cfs-shared-root/ocicfs-e2e-plain.XXXXXX)" ||
		fail "cannot create a fresh ocicfs root"
	"${OCICFS_BIN}" --root "${fresh_ocicfs_root}" pull "${delta_ref}" >&2 ||
		fail "plain pull of release N+1 into a fresh store failed"
	info="$(ocicfs_cli inspect --format json "${delta_ref}")" ||
		fail "the ocicfs store does not hold the delta-pulled release N+1"
	plain_info="$("${OCICFS_BIN}" --root "${fresh_ocicfs_root}" inspect --format json "${delta_ref}")" ||
		fail "the fresh store does not hold the plain-pulled release N+1"
	[ "$(jq -r '.composefs.digest' <<<"${info}")" = "$(jq -r '.composefs.digest' <<<"${plain_info}")" ] ||
		fail "the delta pull built a different composefs image than the plain pull"
	[ "$(jq -r '.fsverity' <<<"${info}")" = "$(jq -r '.fsverity' <<<"${plain_info}")" ] ||
		fail "the delta pull's fs-verity digest differs from the plain pull's"
	rm -rf "${fresh_ocicfs_root}" || fail "cannot remove the fresh ocicfs root"
	fresh_ocicfs_root=""
	pass "the delta pull built the composefs image a plain pull builds"

	config="$(jq -r '.config.digest' <<<"${n1_manifest}")"
	layers_absent "${n1_manifest}" || fail "a layer of release N+1 was in containerd's content store before the pull"
	docker pull "${delta_ref}" >/dev/null || fail "docker pull of the delta-pulled image failed"
	docker image inspect "${delta_ref}" >/dev/null || fail "the runtime does not list the delta-pulled image"
	layers_absent "${n1_manifest}" || fail "docker pull fetched a layer of the delta-pulled image"
	content_has "${config}" || fail "image config was not fetched"
	pass "docker pull of the delta-pulled image fetched no layer"

	assert_delta_rootfs
	pass "the runtime read release N+1's delta-fetched files through the snapshotter"

	ocicfs_cli release --holder delta-base "${n_ref}" >&2 || fail "cannot release the delta base"
	ocicfs_cli remove "${n_ref}" >&2 || fail "cannot remove release N"
	delta_base_ref=""
	ocicfs_cli gc >&2 || fail "ocicfs gc after removing release N failed"
	[ ! -e "${old_object}" ] || fail "release N's app/old object survived its removal and collection"
	[ "$(image_bindings "${n1_digest}")" -eq 1 ] || fail "release N+1 lost its composefs image"
	assert_delta_rootfs
	pass "releasing, removing and collecting the delta base kept release N+1 whole"

	docker rmi "${delta_ref}" >/dev/null || fail "docker rmi of the delta-pulled image failed"
	ocicfs_remove_when_released "${delta_ref}"
	ocicfs_cli gc >&2 || fail "ocicfs gc after removing release N+1 failed"
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
	pass "concurrent containers shared one EROFS mount and used two flat overlays"
}

assert_mount_recovery() {
	local nonce="$1" chainfs="$2" output
	docker create --name cfs-e2e-recovery "${image_ref}" >/dev/null || fail "create recovery container"
	umount "${chainfs}" || fail "unmount shared image mount"
	findmnt -M "${chainfs}" >/dev/null 2>&1 && fail "shared image mount survived explicit unmount"
	CFS_ADMIT=preloaded CFS_ROOT="${WARM_CFS_ROOT}" OCICFS_ROOT="${WARM_OCICFS_ROOT}" \
		CFS_FORCE_RESTART=1 "${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		fail "restart snapshotter"
	output="$(docker start -a cfs-e2e-recovery)" || fail "start existing container after mount loss"
	[ "${output}" = "cfs hello ${nonce}" ] || fail "recovered container output: ${output}"
	[ "$(findmnt -n -o FSTYPE -M "${chainfs}")" = erofs ] || fail "Mounts did not restore EROFS mount"
	docker rm cfs-e2e-recovery >/dev/null || fail "remove recovery container"
	pass "an existing container recovered its image mount through Mounts"
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
	pass "image GC removed the chain mount, loop device, and rm-* mount residue"
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
	[ -n "${object}" ] || fail "cannot find /hello in the ocicfs object view"
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
	pass "verity rejected the tampered object when overlayfs opened /hello"
}

# report_layer_applies prints how long the differ spent on each layer of a
# cold pull — feeding it into the image build and, for the last one, also
# finishing the image — from the elapsed time the snapshotter logs per apply.
# Everything else the daemon logs meanwhile belongs to other records.
report_layer_applies() {
	local ref="$1" layers="$2" offset="$3" entries applies count line layer seconds
	entries="$(tail -n "+$((offset + 1))" "${CFS_LOG}" | grep '^{')" ||
		fail "the snapshotter logged nothing for the pull of ${ref}"
	# The daemon may still be writing its last line; a line that is not yet
	# whole JSON is skipped.
	applies="$(jq -cR 'fromjson? | select(.msg == "cfs differ: applied staged layer")' <<<"${entries}")"
	count="$(grep -c '^{' <<<"${applies}")" || true
	[ "${count}" -eq "${layers}" ] ||
		fail "the snapshotter logged ${count} layer applies for the pull of ${ref}, want ${layers}"
	while IFS= read -r line; do
		layer="$(jq -r '.layer' <<<"${line}")"
		seconds="$(jq -r '.elapsed * 1000 | round / 1000' <<<"${line}")"
		if [ "$(jq -r '.leaf' <<<"${line}")" = true ]; then
			echo "finish: ${ref} layer ${layer} in ${seconds}s"
		else
			echo "apply: ${ref} layer ${layer} in ${seconds}s"
		fi
	done <<<"${applies}"
}

# hello_in_store reports whether the store holds an object with the seeded
# /hello of the image in layout.
hello_in_store() {
	[ -d "$(cas_dir)" ] || return 1
	[ -n "$(hello_objects "$(cas_dir)" "$1" | sed -n '1p')" ]
}

assert_cold_pull_lands_composefs() {
	local nonce="$1" repo="cfs/e2e-cold" layout="${work}/cold-layout"
	local tag_ref offset info root composefs fsverity object view
	local bindings uppers records output pid chainfs

	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-cold" "${tag_ref}" 3 "${layout}" "" "${PARTIAL_LAYER_BYTES}"
	cold_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the cold image ref"
	root="$(manifest_digest "${layout}")"
	# What the store holds before the pull is what it must hold again once the
	# runtime has dropped the image.
	cold_store_bytes="$(cas_object_bytes)"

	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${cold_ref}" >/dev/null 2>&1 &
	pull_pid=$!
	# Layers reach the store as the differ applies them, not once the pull is
	# over: the seeded /hello, in the first layer, must be a store object
	# while the last, large layer is still being transferred. The store
	# holds the image's metadata from the first apply on, so only a known
	# file's object is evidence that a layer went in.
	wait_until "the first layer of ${cold_ref} to reach the store" 600 0.1 \
		hello_in_store "${layout}"
	if docker image inspect "${cold_ref}" >/dev/null 2>&1; then
		fail "the cold pull completed before its first layer was seen in the store"
	fi
	wait "${pull_pid}" || fail "cold docker pull failed"
	pull_pid=""
	report_layer_applies "${cold_ref}" 3 "${offset}"

	# A staged image is the runtime's, not an agent's: the store keeps it by
	# the snapshotter's hold and names it by a runtime reference for that
	# holder, so it resolves by name like any image without being pinned by
	# the name.
	info="$(ocicfs_cli inspect --format json "${cold_ref}")" ||
		fail "the ocicfs store does not resolve the cold-pulled image by its reference"
	[ "$(jq -r '.root' <<<"${info}")" = "${root}" ] ||
		fail "the ocicfs store holds the cold-pulled image under another root"
	[ "$(ocicfs_ref_of_digest "${root}")" = "${cold_ref}" ] ||
		fail "the staged image is not listed under the runtime's reference"
	case "$(ocicfs_holder_of_digest "${root}")" in
	cfs-build-*) ;;
	*) fail "the staged image's reference is not a runtime reference for a cfs build holder" ;;
	esac
	# The pull wrote three versions of shadow.txt; the image references the
	# last. A finished pull leaves the other two for the next collection.
	[ -e "$(shadow_object "${nonce}-cold" 1)" ] ||
		fail "the first layer's shadowed file was collected by the pull itself"
	[ -e "$(shadow_object "${nonce}-cold" 2)" ] ||
		fail "the second layer's shadowed file was collected by the pull itself"
	wait_until "the store to be collected" 60 0.5 collect_store
	[ ! -e "$(shadow_object "${nonce}-cold" 1)" ] ||
		fail "the first layer's shadowed file survived the collection"
	[ ! -e "$(shadow_object "${nonce}-cold" 2)" ] ||
		fail "the second layer's shadowed file survived the collection"
	[ -e "$(shadow_object "${nonce}-cold" 3)" ] ||
		fail "the last layer's file, which the image references, was collected"

	composefs="$(jq -r '.composefs.digest' <<<"${info}")"
	cold_composefs="${composefs}"
	fsverity="$(jq -r '.fsverity' <<<"${info}")"
	object="$(sharded_path "$(cas_dir)" "${composefs}")"
	[ -f "${object}" ] || fail "the composefs image is missing from the store at ${object}"
	view="$(sharded_path "$(verity_view_dir)" "${fsverity}")"
	[ -f "${view}" ] || fail "the composefs image has no fs-verity view entry at ${view}"

	bindings="$(image_bindings "${root}")"
	[ "${bindings}" -eq 1 ] || fail "the cold pull built ${bindings} composefs images, want 1"

	uppers="$(staging_uppers)"
	[ "${uppers}" -eq 0 ] || fail "${uppers} staged upper directories were created for the pull"
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
	pass "a plain docker pull was ingested layer by layer into one composefs image and ran"
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
	pass "re-pulling a converted image refetched its layers and kept one image"
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
	# rather than being answered from the ocicfs store.
	seed_image "${share_nonce}" "${REGISTRY}/${repo}:${nonce}" 3 "${layout}"
	share_ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the shared-base ref"
	docker pull "${share_ref}" >/dev/null || fail "cold docker pull of the shared-base image failed"
	ocicfs_cli inspect --format json "$(manifest_digest "${layout}")" >/dev/null ||
		fail "the ocicfs store does not hold the shared-base image"
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
	ocicfs_cli inspect --format json "$(manifest_digest "${more_layout}")" >/dev/null ||
		fail "the ocicfs store does not hold the prefix-sharing image"
	[ "$(image_bindings "$(manifest_digest "${more_layout}")")" -eq 1 ] ||
		fail "the prefix-sharing pull did not build exactly one composefs image"

	after_bytes="$(cas_object_bytes)"
	[ "${after_bytes}" -lt "$((before_bytes * 2))" ] ||
		fail "the store grew from ${before_bytes} to ${after_bytes} bytes; the shared layers were stored twice"
	copies="$(hello_objects "$(cas_dir)" "${more_layout}" | wc -l)"
	[ "${copies}" -eq 1 ] || fail "the store holds ${copies} copies of the shared /hello, want 1"
	pass "a prefix-sharing image cold-pulled after a base re-pull and shared its objects"

	# Shared content follows the last image that needs it: dropping the base
	# reclaims nothing the longer image reads, and that image still mounts —
	# its chain is resolved from the store's bindings, not from a reference,
	# which no staged image has. The base's whole chain is a prefix of the
	# longer image's, so containerd keeps the base's leaf record as the
	# other's ancestor and the base stays in the store, held by that record,
	# until the longer image goes. Dropping the longer image reclaims both.
	docker rmi "${share_ref}" >/dev/null || fail "docker rmi of the shared-base image failed"
	[ "$(image_bindings "$(manifest_digest "${layout}")")" -eq 1 ] ||
		fail "the shared-base image was reclaimed while its chain still stands under the longer image"
	copies="$(hello_objects "$(cas_dir)" "${more_layout}" | wc -l)"
	[ "${copies}" -eq 1 ] || fail "removing the base left ${copies} copies of the shared /hello, want 1"
	output="$(docker run --rm --network=none "${share_more_ref}")" ||
		fail "docker run of the prefix-sharing image after the base was removed failed"
	[ "${output}" = "cfs hello ${share_nonce}" ] || fail "unexpected container output: ${output}"
	docker rmi "${share_more_ref}" >/dev/null || fail "docker rmi of the prefix-sharing image failed"
	no_image_binding "$(manifest_digest "${more_layout}")" ||
		fail "the prefix-sharing image survived its removal from the runtime"
	no_image_binding "$(manifest_digest "${layout}")" ||
		fail "the shared-base image survived the removal of the last image standing on its chain"
	copies="$(hello_objects "$(cas_dir)" "${more_layout}" | wc -l)"
	[ "${copies}" -eq 0 ] || fail "the shared /hello survived the removal of every image reading it"
	pass "removing one of two images sharing a chain kept the other's content, removing both reclaimed it"
}

# assert_rootfs_is_base reads a container's rootfs the way the runtime serves
# it — an export goes through the snapshotter's mounts — and checks it is the
# base image's and not its derived image's: the derived image's extra layer
# adds layer-<n+1>.txt and rewrites shadow.txt, so a rootfs served from the
# derived chain shows both.
assert_rootfs_is_base() {
	local ref="$1" layers="$2" nonce="$3" tar="${work}/pair-rootfs.tar" listing shadow
	docker create --network=none --name cfs-e2e-pair "${ref}" /hello >/dev/null ||
		fail "cannot create a container from the base image"
	docker export -o "${tar}" cfs-e2e-pair || fail "cannot export the base container's rootfs"
	docker rm cfs-e2e-pair >/dev/null || fail "cannot remove the exported base container"
	listing="$(tar -tf "${tar}")" || fail "cannot list the exported base rootfs"
	grep -Fxq "layer-${layers}.txt" <<<"${listing}" || fail "the base rootfs lacks its own last layer"
	! grep -Fxq "layer-$((layers + 1)).txt" <<<"${listing}" ||
		fail "the base rootfs carries the derived image's extra layer"
	shadow="$(tar -xOf "${tar}" shadow.txt | head -n1)" || fail "cannot read shadow.txt from the base rootfs"
	[ "${shadow}" = "cfs e2e shadow ${layers} ${nonce}" ] ||
		fail "shadow.txt reads \"${shadow}\", want the base image's version"
}

# assert_base_after_derived pulls a base image after an image derived from it
# — one layer longer, seeded from the same nonce, so the base's whole chain
# is a prefix of the derived one's — and runs the base. The base's own pull
# never reaches the snapshotter: containerd finds every chain of the base
# recorded by the derived pull and reports it unpacked without a handshake,
# fetching no layer. What the base's first container meets is therefore the
# derived pull's record of that chain. In preloaded mode that record is a
# chain member and the container mounts whatever the store holds under the
# chain: the base, when the agent pulled it, otherwise nothing — the run
# fails until the agent pulls the base, with no restart and no re-pull. In
# admit-any mode, a base the agent pulled is adopted by the derived pull as it
# applies the layer at the base's chain: that record holds the stored base
# and is a chain member from its commit, and nothing is promoted. A base the
# agent did not pull leaves a staged record that was never built, and the
# first container promotes it: the base is built from the content store and
# held by its own record from then on. Either way the base outlives the
# derived image and goes with its own removal. prepull is whether the
# agent pulled the base before the runtime did. The derived image enters the
# store the way its mode admits: pulled by the agent out of band, built by
# the runtime's own pull in admit-any mode — and by the agent as well when the
# base is, so an agent's pair is one pair.
assert_base_after_derived() {
	local nonce="$1" mode="$2" prepull="$3" layers=2
	local pair_nonce="${nonce}-${mode}-pre${prepull}" flavor="not pre-pulled"
	local base_repo="cfs/e2e-pair-base" derived_repo="cfs/e2e-pair-derived"
	local base_layout="${work}/pair-base-${mode}-${prepull}"
	local derived_layout="${work}/pair-derived-${mode}-${prepull}"
	local base_root derived_root base_first derived_first offset applies digest output
	[ "${prepull}" = 0 ] || flavor="pre-pulled"

	seed_image "${pair_nonce}" "${REGISTRY}/${base_repo}:${pair_nonce}" "${layers}" "${base_layout}"
	seed_image "${pair_nonce}" "${REGISTRY}/${derived_repo}:${pair_nonce}" "$((layers + 1))" "${derived_layout}"
	pair_base_ref="$(pinned_ref "${REGISTRY}/${base_repo}" "${base_layout}")" || fail "cannot pin the pair's base ref"
	pair_derived_ref="$(pinned_ref "${REGISTRY}/${derived_repo}" "${derived_layout}")" ||
		fail "cannot pin the pair's derived ref"
	base_root="$(manifest_digest "${base_layout}")" || fail "cannot read the base manifest digest"
	derived_root="$(manifest_digest "${derived_layout}")" || fail "cannot read the derived manifest digest"
	base_first="$(first_diff_id "${base_layout}")" || fail "cannot read the base diffID"
	derived_first="$(first_diff_id "${derived_layout}")" || fail "cannot read the derived diffID"
	[ "${base_first}" = "${derived_first}" ] || fail "the base is not a prefix of the derived image"

	pair_store="${ocicfs_root}"
	if [ "${mode}" = preloaded ] || [ "${prepull}" = 1 ]; then
		pair_agent_refs="${derived_repo}@${derived_root}"
		stage_image "${derived_repo}" "${derived_layout}"
	fi
	if [ "${prepull}" = 1 ]; then
		pair_agent_refs="${pair_agent_refs} ${base_repo}@${base_root}"
		stage_image "${base_repo}" "${base_layout}"
	fi
	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${pair_derived_ref}" >/dev/null || fail "docker pull of the derived image failed"
	[ "$(image_bindings "${derived_root}")" -eq 1 ] ||
		fail "the derived image's pull did not leave exactly one composefs image"
	if [ "${mode}" = any ] && [ "${prepull}" = 1 ]; then
		tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs differ: adopted stored image for chain"' ||
			fail "the derived image's pull did not adopt the stored base at its chain"
	else
		! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs differ: adopted stored image for chain"' ||
			fail "the derived image's pull adopted a stored image although the base was not stored"
	fi

	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${pair_base_ref}" >/dev/null || fail "docker pull of the base after the derived image failed"
	applies="$(tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -c '"cfs differ: applied staged layer"' || true)"
	[ "${applies}" -eq 0 ] || fail "the base's pull applied ${applies} layers although its chain was recorded"
	if [ "${mode}" = preloaded ]; then
		while read -r digest; do
			content_has "${digest}" && fail "a layer was fetched: ${digest}"
		done < <(jq -r '.layers[].digest' "${derived_layout}/blobs/sha256/${derived_root#sha256:}")
	fi
	if [ "${prepull}" = 1 ]; then
		[ "$(image_bindings "${base_root}")" -eq 1 ] || fail "the base's pull built a second composefs image"
	else
		no_image_binding "${base_root}" || fail "the base's pull built it although its chain was recorded"
		if [ "${mode}" = preloaded ]; then
			if output="$(docker run --network=none --name cfs-e2e-pair "${pair_base_ref}" 2>&1)"; then
				fail "docker run of a base the store does not hold succeeded: ${output}"
			fi
			grep -Fq 'pull it with ocicfs before running the container' <<<"${output}" ||
				fail "docker run of the unheld base did not fail at the chain mount: ${output}"
			if docker container inspect cfs-e2e-pair >/dev/null 2>&1; then
				docker rm -f cfs-e2e-pair >/dev/null || fail "cannot remove the failed base container"
			fi
			no_image_binding "${base_root}" || fail "the failed run built the base into the store"
			pair_agent_refs="${pair_agent_refs} ${base_repo}@${base_root}"
			stage_image "${base_repo}" "${base_layout}"
		fi
	fi

	output="$(docker run --rm --network=none "${pair_base_ref}")" ||
		fail "docker run of the base pulled after the derived image failed"
	[ "${output}" = "cfs hello ${pair_nonce}" ] || fail "unexpected container output: ${output}"
	assert_rootfs_is_base "${pair_base_ref}" "${layers}" "${pair_nonce}"
	[ "$(image_bindings "${base_root}")" -eq 1 ] || fail "the base is not exactly one composefs image in the store"
	if [ "${mode}" = any ] && [ "${prepull}" = 0 ]; then
		tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs snapshotter: promoted recorded chain"' ||
			fail "the base's first container did not promote its chain record"
		[ -n "$(ocicfs_holder_of_digest "${base_root}")" ] ||
			fail "the promoted base is not held by a runtime reference"
	else
		! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs snapshotter: promoted recorded chain"' ||
			fail "a chain record was promoted although the base was stored"
	fi
	if [ "${mode}" = any ] && [ "${prepull}" = 1 ]; then
		# The adopted record holds the agent's base, so it cannot be removed.
		assert_ocicfs_remove_declines "${base_repo}@${base_root}"
	fi

	docker rmi "${pair_derived_ref}" >/dev/null || fail "docker rmi of the derived image failed"
	[ "$(image_bindings "${base_root}")" -eq 1 ] || fail "the base was reclaimed with the derived image"
	output="$(docker run --rm --network=none "${pair_base_ref}")" ||
		fail "docker run of the base after the derived image was removed failed"
	[ "${output}" = "cfs hello ${pair_nonce}" ] || fail "unexpected container output: ${output}"
	docker rmi "${pair_base_ref}" >/dev/null || fail "docker rmi of the base failed"
	if [ "${mode}" = any ] && [ "${prepull}" = 0 ]; then
		no_image_binding "${base_root}" || fail "the promoted base survived its removal from the runtime"
		no_image_binding "${derived_root}" || fail "the derived image survived its removal from the runtime"
	else
		ocicfs_remove_when_released "${base_repo}@${base_root}"
		ocicfs_remove_when_released "${derived_repo}@${derived_root}"
	fi
	pair_base_ref=""
	pair_derived_ref=""
	pair_agent_refs=""
	pass "base ${flavor}: a base pulled after its derived image ran as the base"
}

# assert_staged_rmi_reclaims covers the runtime dropping a staged image: the
# removal releases the snapshotter's hold and collects the store inside the
# same docker command, so by the time `docker rmi` returns the image is gone
# from the runtime, from the store's listing and from disk — with no wait,
# no poll, and no ocicfs command run by anyone.
assert_staged_rmi_reclaims() {
	local root bytes
	root="$(manifest_digest "${work}/cold-layout")"
	ocicfs_lists_digest "${root}" || fail "the ocicfs store does not list the staged image"
	docker rmi "${cold_ref}" >/dev/null || fail "docker rmi of the converted image failed"
	if docker image inspect "${cold_ref}" >/dev/null 2>&1; then
		fail "the runtime still lists the image after docker rmi returned"
	fi
	if ocicfs_lists_digest "${root}"; then
		fail "the ocicfs store still lists the image after docker rmi returned"
	fi
	if ocicfs_cli inspect --format json "${cold_ref}" >/dev/null 2>&1; then
		fail "the runtime's reference survived docker rmi"
	fi
	no_image_binding "${root}" || fail "the composefs image survived docker rmi"
	bytes="$(cas_object_bytes)"
	[ "${bytes}" -eq "${cold_store_bytes}" ] ||
		fail "the store holds ${bytes} object bytes after docker rmi, ${cold_store_bytes} before the pull"
	pass "docker rmi of a staged image reclaimed it from the store before returning"
}

# assert_staged_adoption covers an agent adopting a staged image with a hold
# of its own: the runtime dropping the image releases only the snapshotter's
# hold, and the image stays until the agent lets go and collects.
assert_staged_adoption() {
	local nonce="$1" repo="cfs/e2e-adopt" layout="${work}/adopt-layout" ref root
	seed_image "${nonce}-adopt" "${REGISTRY}/${repo}:${nonce}" 2 "${layout}"
	ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the adopted image ref"
	docker pull "${ref}" >/dev/null || fail "docker pull of the image to adopt failed"
	root="$(manifest_digest "${layout}")"
	ocicfs_cli hold --holder agent "${root}" >&2 || fail "ocicfs hold of the staged image failed"
	docker rmi "${ref}" >/dev/null || fail "docker rmi of the adopted image failed"
	ocicfs_lists_digest "${root}" || fail "the agent-held image was dropped with the runtime's"
	[ "$(image_bindings "${root}")" -eq 1 ] || fail "the adopted image lost its composefs image"
	ocicfs_cli release --holder agent "${root}" >&2 || fail "ocicfs release of the adopted image failed"
	ocicfs_cli gc >&2 || fail "ocicfs gc after the release failed"
	if ocicfs_lists_digest "${root}"; then
		fail "the image survived the agent's release and collection"
	fi
	pass "an agent's hold kept a staged image past docker rmi, its release let it go"
}

# assert_staged_self_heal covers a hold of the snapshotter's own that no
# record owns — what a crash between a build's finish and its record leaves
# behind: the daemon releases it when it starts, so the image is reclaimed
# by the runtime's removal like any other rather than pinned for good.
assert_staged_self_heal() {
	local nonce="$1" repo="cfs/e2e-heal" layout="${work}/heal-layout" ref root
	local stray="cfs-build-0000000000000000000000000000000000000000000000000000000000000000"
	seed_image "${nonce}-heal" "${REGISTRY}/${repo}:${nonce}" 2 "${layout}"
	ref="$(pinned_ref "${REGISTRY}/${repo}" "${layout}")" || fail "cannot pin the self-heal image ref"
	docker pull "${ref}" >/dev/null || fail "docker pull of the self-heal image failed"
	root="$(manifest_digest "${layout}")"
	ocicfs_cli hold --holder "${stray}" "${root}" >&2 || fail "cannot plant the stray hold"
	CFS_INGEST_WORKERS=1 CFS_ADMIT=any CFS_ROOT="${COLD_CFS_ROOT}" OCICFS_ROOT="${COLD_OCICFS_ROOT}" \
		CFS_FORCE_RESTART=1 "${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "restart the snapshotter"
	grep -q "releasing a hold no snapshot record owns" "${CFS_LOG}" ||
		fail "the restarted snapshotter did not report releasing the stray hold"
	docker rmi "${ref}" >/dev/null || fail "docker rmi of the self-heal image failed"
	if ocicfs_lists_digest "${root}"; then
		fail "the stray hold kept the image past docker rmi"
	fi
	pass "a hold no record owned was released at startup and the image reclaimed"
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

	wait_until "the aborted pull's active snapshot to go" 60 1 active_gone_after_lease_drop "${leases}"
	if docker image inspect "${partial_ref}" >/dev/null 2>&1; then
		fail "the cold pull completed before it could be interrupted"
	fi
	wait_until "the aborted pull's staged snapshots to be collected" 120 1 \
		collected_after_lease_drop "${leases}" "${baseline}"
	assert_no_staging_residue
	# Removing the pull's snapshots closed its image build, which held the
	# store's shared lock while it was open, and had the snapshotter collect
	# what the build stored; a collection run by hand afterwards runs instead
	# of reporting the store busy, and finds nothing left.
	wait_until "the abandoned build's leftovers to be collected" 60 0.5 store_settled
	ocicfs_cli gc >&2 || fail "ocicfs gc after the aborted pull did not run"

	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${partial_ref}" >/dev/null || fail "re-pull after the interrupted pull failed"
	report_layer_applies "${partial_ref}" 3 "${offset}"
	ocicfs_cli inspect --format json "$(manifest_digest "${layout}")" >/dev/null ||
		fail "the re-pull did not land in the ocicfs store"
	[ "$(image_bindings "$(manifest_digest "${layout}")")" -eq 1 ] ||
		fail "the re-pull did not build exactly one composefs image"
	assert_no_staging_residue
	pass "an interrupted cold pull left nothing behind and a re-pull converged"
}

# assert_cold_mixed_door covers the two doors meeting on one image: the
# runtime stages a tag pull, holds the platform manifest it stamped and names
# it by a runtime reference; an ocicfs pull of the same tag resolves its index
# down to that same manifest and adopts the image — replacing the runtime's
# reference with the agent's, not adding a second image — and an ocicfs remove
# of the tag declines on the runtime's hold instead of orphaning it.
assert_cold_mixed_door() {
	local nonce="$1" repo="cfs/e2e-mixed" layout="${work}/mixed-layout"
	local tag_ref idx mdigest output
	tag_ref="${REGISTRY}/${repo}:${nonce}"
	seed_image "${nonce}-mixed" "${tag_ref}" 2 "${layout}" "" "" 1
	idx="$(manifest_digest "${layout}")" || fail "cannot read the index digest"
	mdigest="$(indexed_manifest_digest "${layout}")" || fail "cannot read the platform manifest digest"
	[ "${idx}" != "${mdigest}" ] || fail "the seeded tag does not resolve through an index"

	docker pull "${tag_ref}" >/dev/null || fail "docker pull of the index-rooted tag failed"
	[ "$(image_bindings "${mdigest}")" -eq 1 ] ||
		fail "the staged pull did not build exactly one composefs image"
	ocicfs_lists_digest "${mdigest}" || fail "the staged image is not listed under the stamped manifest"
	[ "$(ocicfs_ref_of_digest "${mdigest}")" = "${tag_ref}" ] ||
		fail "the staged pull did not name the image by the runtime's reference"
	[ -n "$(ocicfs_holder_of_digest "${mdigest}")" ] ||
		fail "the runtime's reference is not a runtime reference"

	ocicfs_cli pull "${tag_ref}" >&2 || fail "ocicfs pull of the staged tag failed"
	[ "$(image_bindings "${mdigest}")" -eq 1 ] ||
		fail "the ocicfs pull built a second composefs image"
	no_image_binding "${idx}" || fail "the ocicfs pull built a binding under the index digest"
	[ "$(ref_record_field "${tag_ref}" '.identity.digest')" = "${mdigest}" ] ||
		fail "the ocicfs pull re-keyed the image away from the manifest identity"
	[ "$(ocicfs_ref_of_digest "${mdigest}")" = "${tag_ref}" ] ||
		fail "the ocicfs pull did not name the held image"
	[ -z "$(ocicfs_holder_of_digest "${mdigest}")" ] ||
		fail "the ocicfs pull left the runtime's reference in place instead of adopting the image"
	[ "$(ref_record_field "${tag_ref}" '.resolved.digest')" = "${idx}" ] ||
		fail "the ocicfs pull did not record the index as provenance"

	assert_ocicfs_remove_declines "${tag_ref}"
	output="$(docker run --rm --network=none "${tag_ref}")" ||
		fail "docker run after the declined removal failed"
	[ "${output}" = "cfs hello ${nonce}-mixed" ] || fail "unexpected container output: ${output}"

	docker rmi "${tag_ref}" >/dev/null || fail "docker rmi of the mixed-door image failed"
	ocicfs_remove_when_released "${tag_ref}"
	pass "an ocicfs pull of a runtime-staged tag kept one held image and remove declined"
}

# assert_ingest_workers_converge re-pulls the cold image with the snapshotter
# at two ingest workers and compares the composefs image it builds with the
# one built at one worker, and with the one an ocicfs pull of the same image
# builds from its OCI layout into a fresh store: the worker count and the door
# an image comes in through change the order objects land in the store, never
# the image.
assert_ingest_workers_converge() {
	local layout="${work}/cold-layout" root offset composefs fresh_composefs
	root="$(manifest_digest "${layout}")" || fail "cannot read the cold manifest digest"
	[ -n "${cold_composefs}" ] || fail "the one-worker cold pull recorded no composefs digest"

	CFS_INGEST_WORKERS=2 CFS_ADMIT=any CFS_ROOT="${COLD_CFS_ROOT}" OCICFS_ROOT="${COLD_OCICFS_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "restart the snapshotter with two ingest workers"
	offset="$(wc -l <"${CFS_LOG}")"
	docker pull "${cold_ref}" >/dev/null || fail "re-pull at two ingest workers failed"
	report_layer_applies "${cold_ref}" 3 "${offset}"
	composefs="$(ocicfs_cli inspect --format json "${root}" | jq -r '.composefs.digest')" ||
		fail "the ocicfs store does not hold the image re-pulled at two workers"
	[ "${composefs}" = "${cold_composefs}" ] ||
		fail "two ingest workers built ${composefs}, one worker built ${cold_composefs}"

	# The fresh store must sit on the shared fs-verity filesystem, like every
	# other store the devcontainer uses.
	fresh_ocicfs_root="$(mktemp -d /var/lib/cfs-shared-root/ocicfs-e2e-fresh.XXXXXX)" ||
		fail "cannot create a fresh ocicfs root"
	"${OCICFS_BIN}" --root "${fresh_ocicfs_root}" pull --from-dir "${layout}" "${cold_ref}" >&2 ||
		fail "ocicfs pull of the cold image into a fresh store failed"
	fresh_composefs="$("${OCICFS_BIN}" --root "${fresh_ocicfs_root}" inspect --format json "${cold_ref}" |
		jq -r '.composefs.digest')" || fail "cannot inspect the fresh store's image"
	[ "${fresh_composefs}" = "${cold_composefs}" ] ||
		fail "ocicfs pull built ${fresh_composefs}, the snapshotter built ${cold_composefs}"
	[ "${root}" = "$(manifest_digest "${layout}")" ] || fail "the cold layout changed underneath the test"
	pass "one and two ingest workers and an ocicfs pull built the same composefs image"
}

# stop_registry and start_registry take the devcontainer's registry down and
# bring it back the way the entrypoint starts it, so the load legs prove the
# runtime reaches nothing over the network.
stop_registry() {
	local pid
	pid="$(cat /var/log/cfs-devcontainer/zot.pid)" || fail "cannot read the registry's pid"
	kill "${pid}" || fail "cannot stop the registry"
	registry_down=1
	wait_until "the registry to stop" 30 0.5 registry_unreachable
}

start_registry() {
	setsid nohup zot serve /etc/zot/config.json >>/var/log/cfs-devcontainer/zot.log 2>&1 &
	echo "$!" >/var/log/cfs-devcontainer/zot.pid
	local tries
	for tries in $(seq 1 60); do
		if curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1; then
			registry_down=""
			return 0
		fi
		sleep 0.5
	done
	return 1
}

registry_unreachable() {
	! curl -fsS -m 2 "http://${REGISTRY}/v2/" >/dev/null 2>&1
}

# prepare_load_archives writes the archives the load legs load: a base image
# and an image derived from it — one layer longer, seeded from one nonce — each
# pulled by the runtime and saved by its tag with docker save, so the archive
# names the image. The runtime and the store forget both images afterwards.
prepare_load_archives() {
	local nonce="$1" base_layout="${work}/load-base-layout" derived_layout="${work}/load-derived-layout"
	load_nonce="${nonce}-load"
	load_base_tag="${REGISTRY}/cfs/e2e-load-base:${load_nonce}"
	load_derived_tag="${REGISTRY}/cfs/e2e-load-derived:${load_nonce}"
	load_base_tar="${work}/load-base.tar"
	load_derived_tar="${work}/load-derived.tar"
	seed_image "${load_nonce}" "${load_base_tag}" 2 "${base_layout}"
	seed_image "${load_nonce}" "${load_derived_tag}" 3 "${derived_layout}"
	[ "$(first_diff_id "${base_layout}")" = "$(first_diff_id "${derived_layout}")" ] ||
		fail "the load base is not a prefix of the derived image"
	docker pull "${load_base_tag}" >/dev/null || fail "docker pull of the load base failed"
	docker pull "${load_derived_tag}" >/dev/null || fail "docker pull of the load derived image failed"
	docker save -o "${load_base_tar}" "${load_base_tag}" || fail "docker save of the load base failed"
	docker save -o "${load_derived_tar}" "${load_derived_tag}" || fail "docker save of the load derived image failed"
	[ "$(archive_root "${load_base_tar}")" = "$(manifest_digest "${base_layout}")" ] ||
		fail "the base archive does not hold the pulled manifest"
	[ "$(archive_root "${load_derived_tar}")" = "$(manifest_digest "${derived_layout}")" ] ||
		fail "the derived archive does not hold the pulled manifest"
	drain_runtime_images
	forget_ocicfs_images
}

# archive_root prints the digest of the one image a docker save archive holds.
archive_root() {
	tar -xOf "$1" index.json | jq -r '.manifests[0].digest'
}

# archive_agent_ref prints the reference ocicfs pull --from-tar records for an
# archive given no reference: the name docker save wrote, pinned to the
# archive's image.
archive_agent_ref() {
	local tag="$1" archive="$2"
	printf '%s@%s\n' "${tag%:*}" "$(archive_root "${archive}")"
}

# sideload puts an archive's image in the store the way an agent does,
# offline, and checks the reference it records.
sideload() {
	local tag="$1" archive="$2" ref got
	ref="$(archive_agent_ref "${tag}" "${archive}")"
	got="$(ocicfs_cli pull --from-tar "${archive}" --format json | jq -r '.ref')" ||
		fail "ocicfs pull --from-tar of ${archive} failed"
	[ "${got}" = "${ref}" ] || fail "ocicfs pull --from-tar recorded ${got}, want ${ref}"
	pair_agent_refs="${pair_agent_refs:+${pair_agent_refs} }${ref}"
}

# check_load runs docker load, which exits 0 even when it cannot unpack the
# image it loaded, and fails on either.
check_load() {
	local archive="$1" output
	if ! output="$(docker load -i "${archive}" 2>&1)"; then
		echo "${output}" >&2
		return 1
	fi
	echo "${output}" >&2
	! grep -q 'Error unpacking image' <<<"${output}"
}

# loaded_applies prints the layers the differ resolved for a load since a log
# offset, one "<leaf> <source>" line per layer. A line the daemon has not
# finished writing is skipped.
loaded_applies() {
	local offset="$1"
	tail -n "+$((offset + 1))" "${CFS_LOG}" |
		jq -rR 'fromjson? | select(.msg == "cfs differ: applied loaded layer") | "\(.leaf) \(.source)"'
}

# assert_load_declined covers docker load of an image the store does not
# hold on a daemon that builds nothing from runtime content: the load reports
# the unpack failure naming the sideload, a run of the loaded image fails the
# same way, and nothing is built.
assert_load_declined() {
	local output root
	root="$(archive_root "${load_derived_tar}")"
	output="$(docker load -i "${load_derived_tar}" 2>&1)" || fail "docker load exited non-zero: ${output}"
	grep -q 'Error unpacking image' <<<"${output}" ||
		fail "docker load of an image the store lacks unpacked it: ${output}"
	grep -Fq 'ocicfs pull --from-tar' <<<"${output}" ||
		fail "docker load did not say to sideload the image: ${output}"
	if output="$(docker run --rm --network=none "${load_derived_tag}" 2>&1)"; then
		fail "docker run of an image the store lacks succeeded: ${output}"
	fi
	no_image_binding "${root}" || fail "the declined load built the image"
	docker rmi "${load_derived_tag}" >/dev/null || fail "docker rmi of the declined image failed"
	wait_until "the declined load's records to go" 60 0.5 no_cfs_snapshots
	pass "docker load of an image the store lacks was declined, naming the sideload"
}

# assert_load_built covers docker load alone on a staging daemon: nothing is
# sideloaded, so the unpack finds no chain in the store and builds the image
# from the copy the load put in containerd's content store — the layers below
# the last answered from the manifest, the last building the image — and the
# image runs with no network. The runtime's hold is the image's only one, so
# docker rmi reclaims it from the store before returning.
assert_load_built() {
	local root offset applies output
	root="$(archive_root "${load_derived_tar}")"
	offset="$(wc -l <"${CFS_LOG}")"
	check_load "${load_derived_tar}" || fail "docker load of an image the store lacks failed"
	applies="$(loaded_applies "${offset}")"
	[ "${applies}" = "$(printf 'false content\nfalse content\ntrue content')" ] ||
		fail "the load resolved its layers as \"${applies}\", want two answered and the last built from the runtime's content"
	! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs differ: applied staged layer"' ||
		fail "the load went through the pull path"
	output="$(docker run --rm --network=none "${load_derived_tag}")" ||
		fail "docker run of the loaded image failed"
	[ "${output}" = "cfs hello ${load_nonce}" ] || fail "unexpected container output: ${output}"
	[ "$(image_bindings "${root}")" -eq 1 ] || fail "the store does not hold exactly one composefs image"
	docker rmi "${load_derived_tag}" >/dev/null || fail "docker rmi of the loaded image failed"
	if ocicfs_lists_digest "${root}"; then
		fail "the ocicfs store still lists the loaded image after docker rmi returned"
	fi
	no_image_binding "${root}" || fail "the composefs image survived docker rmi"
	wait_until "the loaded image's records to go" 60 0.5 no_cfs_snapshots
	pass "an image loaded with docker load alone was built from its content, ran offline and was reclaimed by docker rmi"
}

# assert_load_from_store covers the offline recipe: the agent sideloads the
# archive, then docker loads it. Every layer is resolved from the store —
# the ones below the last answered, the last adopting the stored image — the
# image runs with no network, the agent cannot remove it while the runtime
# holds it, and it is the agent's to remove once the runtime lets go.
assert_load_from_store() {
	local mode="$1" root ref offset applies output
	root="$(archive_root "${load_derived_tar}")"
	ref="$(archive_agent_ref "${load_derived_tag}" "${load_derived_tar}")"
	sideload "${load_derived_tag}" "${load_derived_tar}"
	offset="$(wc -l <"${CFS_LOG}")"
	check_load "${load_derived_tar}" || fail "docker load of the sideloaded image failed"
	applies="$(loaded_applies "${offset}")"
	[ "${applies}" = "$(printf 'false store\nfalse store\ntrue store')" ] ||
		fail "the load resolved its layers as \"${applies}\", want two answered and the last adopted from the store"
	! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs differ: applied staged layer"' ||
		fail "the load built a layer although the store held the image"
	output="$(docker run --rm --network=none "${load_derived_tag}")" ||
		fail "docker run of the loaded image failed"
	[ "${output}" = "cfs hello ${load_nonce}" ] || fail "unexpected container output: ${output}"
	[ "$(image_bindings "${root}")" -eq 1 ] || fail "the store does not hold exactly one composefs image"
	[ -z "$(ocicfs_holder_of_digest "${root}")" ] || fail "the load recorded a runtime reference"
	assert_ocicfs_remove_declines "${ref}"
	docker rmi "${load_derived_tag}" >/dev/null || fail "docker rmi of the loaded image failed"
	ocicfs_remove_when_released "${ref}"
	pair_agent_refs=""
	wait_until "the loaded image's records to go" 60 0.5 no_cfs_snapshots
	pass "a sideloaded image loaded with docker load ran offline from the store"
}

# assert_load_base_after_derived loads a base image after an image derived
# from it. The base's load finds every chain of the base recorded by the
# derived image's and unpacks nothing, so the base's first container meets
# the derived load's record of the base's chain. With both sideloaded before
# the loads (store=both) that record adopted the stored base and the base
# runs from it as it is; with the base sideloaded only between the loads
# (store=late) the record answered as a prefix of the derived image, and the
# base's first container promotes it by adopting the stored base. With
# nothing sideloaded (store=none, admit-any mode only) the derived load built
# the derived image from its content and answered the base's chain as a
# layer of it, and the base's first container promotes the record by
# building the base from the content its load left. Every way the base runs
# as the base, offline, and outlives the derived image.
assert_load_base_after_derived() {
	local mode="$1" store="$2" offset output ref base_root derived_root
	base_root="$(archive_root "${load_base_tar}")"
	derived_root="$(archive_root "${load_derived_tar}")"
	if [ "${store}" != none ]; then
		sideload "${load_derived_tag}" "${load_derived_tar}"
	fi
	if [ "${store}" = both ]; then
		sideload "${load_base_tag}" "${load_base_tar}"
	fi
	offset="$(wc -l <"${CFS_LOG}")"
	check_load "${load_derived_tar}" || fail "docker load of the derived image failed"
	case "${store}" in
	both)
		[ "$(loaded_applies "${offset}")" = "$(printf 'false store\ntrue store\ntrue store')" ] ||
			fail "the derived load did not adopt the stored base at its chain: $(loaded_applies "${offset}")"
		;;
	late)
		[ "$(loaded_applies "${offset}")" = "$(printf 'false store\nfalse store\ntrue store')" ] ||
			fail "the derived load did not answer the base's chain as a prefix: $(loaded_applies "${offset}")"
		sideload "${load_base_tag}" "${load_base_tar}"
		;;
	none)
		[ "$(loaded_applies "${offset}")" = "$(printf 'false content\nfalse content\ntrue content')" ] ||
			fail "the derived load did not build the derived image from its content: $(loaded_applies "${offset}")"
		;;
	esac
	offset="$(wc -l <"${CFS_LOG}")"
	check_load "${load_base_tar}" || fail "docker load of the base after the derived image failed"
	[ -z "$(loaded_applies "${offset}")" ] || fail "the base's load applied layers although its chain was recorded"

	output="$(docker run --rm --network=none "${load_base_tag}")" ||
		fail "docker run of the base loaded after the derived image failed"
	[ "${output}" = "cfs hello ${load_nonce}" ] || fail "unexpected container output: ${output}"
	assert_rootfs_is_base "${load_base_tag}" 2 "${load_nonce}"
	case "${store}" in
	late)
		tail -n "+$((offset + 1))" "${CFS_LOG}" | grep '"cfs snapshotter: promoted recorded chain"' |
			grep -q '"source":"store"' ||
			fail "the base's first container did not promote its chain from the store"
		;;
	none)
		tail -n "+$((offset + 1))" "${CFS_LOG}" | grep '"cfs snapshotter: promoted recorded chain"' |
			grep -q '"source":"content"' ||
			fail "the base's first container did not promote its chain from the runtime's content"
		;;
	both)
		! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs snapshotter: promoted recorded chain"' ||
			fail "a chain record was promoted although the derived load adopted the base"
		;;
	esac
	! tail -n "+$((offset + 1))" "${CFS_LOG}" | grep -q '"cfs differ: applied staged layer"' ||
		fail "a layer of the pair went through the pull path"

	docker rmi "${load_derived_tag}" >/dev/null || fail "docker rmi of the derived image failed"
	output="$(docker run --rm --network=none "${load_base_tag}")" ||
		fail "docker run of the base after the derived image was removed failed"
	[ "${output}" = "cfs hello ${load_nonce}" ] || fail "unexpected container output: ${output}"
	docker rmi "${load_base_tag}" >/dev/null || fail "docker rmi of the base failed"
	for ref in ${pair_agent_refs}; do
		ocicfs_remove_when_released "${ref}"
	done
	pair_agent_refs=""
	wait_until "the pair's records to go" 60 0.5 no_cfs_snapshots
	if [ "${store}" = none ]; then
		no_image_binding "${base_root}" || fail "the base built by its promotion survived docker rmi"
		no_image_binding "${derived_root}" || fail "the derived image built by its load survived docker rmi"
	fi
	pass "base sideloaded ${store}: a base loaded after its derived image ran as the base offline"
}

# assert_sideload_then_pull covers the preloaded alternative to docker
# load: the agent sideloads the archive, and the runtime pulls the image by
# the digest the sideload recorded while the registry is reachable, fetching
# its metadata and no layer, so containerd keeps no copy of the archive's
# layers. A pull by the archive's tag before that is declined, since the
# sideload records no tag. With the registry stopped afterwards, the image
# runs offline.
assert_sideload_then_pull() {
	local root ref manifest output
	root="$(archive_root "${load_derived_tar}")"
	ref="$(archive_agent_ref "${load_derived_tag}" "${load_derived_tar}")"
	manifest="$(tar -xOf "${load_derived_tar}" "blobs/sha256/${root#sha256:}")" ||
		fail "cannot read the archive's manifest"
	sideload "${load_derived_tag}" "${load_derived_tar}"
	# The tag goes first: once the digest pull has unpacked the image, a tag
	# pull finds every chain present and never reaches the snapshotter.
	if output="$(docker pull "${load_derived_tag}" 2>&1)"; then
		fail "docker pull by a tag the sideload did not record succeeded: ${output}"
	fi
	layers_absent "${manifest}" || fail "the declined tag pull fetched a layer"
	load_pulled_ref="${ref}"
	docker pull "${ref}" >/dev/null || fail "docker pull by the sideloaded digest failed"
	layers_absent "${manifest}" || fail "docker pull after the sideload fetched a layer"
	stop_registry
	output="$(docker run --rm --network=none "${ref}")" || fail "docker run of the pulled image failed offline"
	[ "${output}" = "cfs hello ${load_nonce}" ] || fail "unexpected container output: ${output}"
	start_registry || fail "cannot restart the registry"
	docker rmi "${ref}" >/dev/null || fail "docker rmi of the pulled image failed"
	load_pulled_ref=""
	ocicfs_remove_when_released "${ref}"
	pair_agent_refs=""
	wait_until "the pulled image's records to go" 60 0.5 no_cfs_snapshots
	wait_until "the store to collect the pulled image" 60 0.5 collect_store
	no_image_binding "${root}" || fail "the pulled image survived its removal and a collection"
	pass "a sideloaded image pulled by digest fetched no layer and ran offline"
}

# assert_load_offline runs the load legs with the registry stopped, against
# the stores of the phase in progress.
assert_load_offline() {
	local mode="$1"
	drain_runtime_images
	pair_store="${ocicfs_root}"
	if [ "${mode}" = preloaded ]; then
		assert_sideload_then_pull
	fi
	stop_registry
	if [ "${mode}" = preloaded ]; then
		assert_load_declined
	else
		assert_load_built
	fi
	assert_load_from_store "${mode}"
	if [ "${mode}" = any ]; then
		assert_load_base_after_derived "${mode}" none
	fi
	assert_load_base_after_derived "${mode}" late
	assert_load_base_after_derived "${mode}" both
	start_registry || fail "cannot restart the registry"
}

warm_phase() {
	local nonce="$1" chainfs
	section "1. Preloaded mode (-admit=preloaded): images enter the store with ocicfs pull"
	drain_runtime_images
	# The devcontainer boots in admit-any mode; the warm legs need the
	# preloaded posture, pinned explicitly rather than inherited.
	CFS_ADMIT=preloaded CFS_ROOT="${WARM_CFS_ROOT}" OCICFS_ROOT="${WARM_OCICFS_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 ||
		fail "start the snapshotter in preloaded mode"
	forget_ocicfs_images
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
	assert_delta_fast_path
	assert_base_after_derived "${nonce}" preloaded 1
	assert_base_after_derived "${nonce}" preloaded 0
	assert_load_offline preloaded
}

cold_phase() {
	local nonce="$1"
	section "2. Admit-any mode (-admit=any): the runtime's own pull or load fills the store"
	# A device runs in one mode or the other, never both, so the cold phase
	# gets its own snapshotter and ocicfs stores; no record made in one mode is
	# ever read back in the other.
	drain_runtime_images
	# The roots flip before the daemon does: if the cold start dies halfway,
	# cleanup must already see the cold posture to restore the boot one.
	ocicfs_root="${COLD_OCICFS_ROOT}"
	cfs_root="${COLD_CFS_ROOT}"
	# One ingest worker first; the last leg switches to two and compares.
	CFS_INGEST_WORKERS=1 CFS_ADMIT=any CFS_ROOT="${COLD_CFS_ROOT}" OCICFS_ROOT="${COLD_OCICFS_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "start the snapshotter in cold-pull mode"
	# The daemon collects the store once at start, on its own and retried
	# while busy; run-cfs.sh truncated the log, so the first such entry is
	# this daemon's. The cold pull's shadowed-object assertions need that
	# collection to be over, since it may reclaim them.
	wait_until "the snapshotter's startup collection" 120 0.5 \
		grep -q "cfs snapshotter: collected the store" "${CFS_LOG}"
	forget_ocicfs_images

	assert_cold_pull_lands_composefs "${nonce}"
	assert_cold_repull_refetches "${nonce}"
	assert_staged_rmi_reclaims
	assert_cold_prefix_dedup "${nonce}"
	assert_base_after_derived "${nonce}" any 1
	assert_base_after_derived "${nonce}" any 0
	assert_interrupted_pull_converges "${nonce}"
	assert_cold_mixed_door "${nonce}"
	assert_staged_adoption "${nonce}"
	assert_staged_self_heal "${nonce}"
	assert_ingest_workers_converge
	assert_load_offline any

	drain_runtime_images
	CFS_ADMIT=any CFS_ROOT="${WARM_CFS_ROOT}" OCICFS_ROOT="${WARM_OCICFS_ROOT}" \
		"${SCRIPT_DIR}/run-cfs.sh" >&2 || fail "restore the boot snapshotter posture"
	ocicfs_root="${WARM_OCICFS_ROOT}"
	cfs_root="${WARM_CFS_ROOT}"
}

main() {
	if [ ! -x "${OCICFS_BIN}" ]; then
		echo "E2E FAIL: ocicfs CLI not found at ${OCICFS_BIN}; run the e2e from the host as
./devshell.sh make test-e2e — devshell builds the ocicfs CLI there and
mounts it into the container (a container started any other way has none)" >&2
		exit 1
	fi
	work="$(mktemp -d)"
	trap cleanup EXIT
	local nonce="${RANDOM}${RANDOM}"
	prepare_load_archives "${nonce}"
	warm_phase "${nonce}"
	cold_phase "${nonce}"
}

main "$@"
