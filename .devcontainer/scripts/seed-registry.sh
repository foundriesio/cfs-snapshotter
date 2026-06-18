#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

CONTAINERD_SOCKET=${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
REGISTRY_URL=${REGISTRY_URL:-http://127.0.0.1:5000}
IMAGE_REF=${1:-127.0.0.1:5000/cfs/smoke:latest}
SEED_NAMESPACE=${SEED_NAMESPACE:-cfs-seed}

detect_architecture() {
	local arch
	arch="${SMOKE_ARCH:-}"
	if [ -z "${arch}" ] && command -v go >/dev/null 2>&1; then
		arch="$(go env GOARCH)"
	fi
	if [ -z "${arch}" ]; then
		arch="$(uname -m)"
	fi

	case "${arch}" in
		amd64 | x86_64) echo amd64 ;;
		arm64 | aarch64) echo arm64 ;;
		arm | armv7l) echo arm ;;
		*) echo "unsupported smoke image architecture: ${arch}" >&2; return 1 ;;
	esac
}

wait_for_registry() {
	for _ in $(seq 1 30); do
		if curl -fsS "${REGISTRY_URL}/v2/" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done

	echo "timed out waiting for ${REGISTRY_URL}" >&2
	return 1
}

digest_file() {
	sha256sum "$1" | awk '{print $1}'
}

size_file() {
	stat -c '%s' "$1"
}

write_image_layout() {
	local work="$1"
	local architecture="$2"
	local oci="${work}/oci"
	local rootfs="${work}/rootfs"

	mkdir -p "${oci}/blobs/sha256" "${rootfs}"
	printf 'cfs smoke test\n' >"${rootfs}/hello.txt"

	tar -C "${rootfs}" -cf "${work}/layer.tar" .
	gzip -n -c "${work}/layer.tar" >"${work}/layer.tar.gz"

	local layer_diff_id layer_digest layer_size
	layer_diff_id="$(digest_file "${work}/layer.tar")"
	layer_digest="$(digest_file "${work}/layer.tar.gz")"
	layer_size="$(size_file "${work}/layer.tar.gz")"
	cp "${work}/layer.tar.gz" "${oci}/blobs/sha256/${layer_digest}"

	cat >"${work}/config.json" <<EOF
{"created":"1970-01-01T00:00:00Z","architecture":"${architecture}","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["sha256:${layer_diff_id}"]},"history":[{"created":"1970-01-01T00:00:00Z","created_by":"cfs devcontainer smoke image"}]}
EOF

	local config_digest config_size
	config_digest="$(digest_file "${work}/config.json")"
	config_size="$(size_file "${work}/config.json")"
	cp "${work}/config.json" "${oci}/blobs/sha256/${config_digest}"

	cat >"${work}/manifest.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:${config_digest}","size":${config_size}},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:${layer_digest}","size":${layer_size}}]}
EOF

	local manifest_digest manifest_size
	manifest_digest="$(digest_file "${work}/manifest.json")"
	manifest_size="$(size_file "${work}/manifest.json")"
	cp "${work}/manifest.json" "${oci}/blobs/sha256/${manifest_digest}"

	cat >"${oci}/index.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:${manifest_digest}","size":${manifest_size},"platform":{"architecture":"${architecture}","os":"linux"},"annotations":{"org.opencontainers.image.ref.name":"latest"}}]}
EOF
	printf '{"imageLayoutVersion":"1.0.0"}\n' >"${oci}/oci-layout"

	tar -C "${oci}" -cf "${work}/smoke.oci.tar" .
}

main() {
	wait_for_registry

	local work
	work="$(mktemp -d)"
	trap "rm -rf '${work}'" EXIT

	write_image_layout "${work}" "$(detect_architecture)"

	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images import \
		--digests \
		--base-name "${IMAGE_REF%:*}" \
		"${work}/smoke.oci.tar"
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images push \
		--plain-http \
		"${IMAGE_REF}"

	echo "seeded ${IMAGE_REF}"
}

main "$@"
