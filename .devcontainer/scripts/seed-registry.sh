#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${SCRIPT_DIR}/require-devcontainer.sh"
require_cfs_devcontainer

CONTAINERD_SOCKET=${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
REGISTRY_URL=${REGISTRY_URL:-http://127.0.0.1:5000}
IMAGE_REF=${1:-127.0.0.1:5000/cfs/e2e:latest}
SEED_NAMESPACE=${SEED_NAMESPACE:-cfs-seed}
SEED_LAYER_COUNT=${SEED_LAYER_COUNT:-1}
SEED_NONCE=${SEED_NONCE:-}
SEED_KEEP_LAYOUT=${SEED_KEEP_LAYOUT:-}
SEED_RUNNABLE=${SEED_RUNNABLE:-}
SEED_ALTERNATE_CONFIG=${SEED_ALTERNATE_CONFIG:-}
SEED_LAYER_BYTES=${SEED_LAYER_BYTES:-}
SEED_INDEX_ROOT=${SEED_INDEX_ROOT:-}

work=""

detect_architecture() {
	local arch="${SMOKE_ARCH:-}"
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
		*) echo "unsupported e2e image architecture: ${arch}" >&2; return 1 ;;
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

validate_layer_count() {
	case "${SEED_LAYER_COUNT}" in
		'' | *[!0-9]*)
			echo "SEED_LAYER_COUNT must be a positive integer" >&2
			return 1
			;;
	esac
	if [ "${SEED_LAYER_COUNT}" -lt 1 ]; then
		echo "SEED_LAYER_COUNT must be a positive integer" >&2
		return 1
	fi
}

digest_file() {
	sha256sum "$1" | awk '{print $1}'
}

size_file() {
	stat -c '%s' "$1"
}

append_json_item() {
	local current="$1" item="$2"
	if [ -n "${current}" ]; then
		printf '%s,%s' "${current}" "${item}"
	else
		printf '%s' "${item}"
	fi
}

write_layer_rootfs() {
	local rootfs="$1" layer="$2"
	mkdir -p "${rootfs}"
	if [ "${layer}" = 1 ] && [ -n "${SEED_RUNNABLE}" ]; then
		local source_dir
		source_dir="$(mktemp -d)"
		cat >"${source_dir}/hello.go" <<EOF
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		<-term
		return
	}
	if err := os.WriteFile("/scratch.txt", []byte("rw"), 0o644); err != nil {
		os.Stderr.WriteString("write: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("cfs hello ${SEED_NONCE}\n")
}
EOF
		CGO_ENABLED=0 go build -trimpath -o "${rootfs}/hello" "${source_dir}/hello.go"
		rm -rf "${source_dir}" || echo "warning: failed to remove ${source_dir}" >&2
	fi
	printf 'cfs e2e layer %s %s\n' "${layer}" "${SEED_NONCE}" >"${rootfs}/layer-${layer}.txt"
	# Bulk goes in the last layer only, and is incompressible, so a pull of the
	# image spends its transfer time where a test wants a wide window: after
	# the earlier layers have been applied.
	if [ -n "${SEED_LAYER_BYTES}" ] && [ "${layer}" = "${SEED_LAYER_COUNT}" ]; then
		head -c "${SEED_LAYER_BYTES}" /dev/urandom >"${rootfs}/bulk.bin"
	fi
}

write_image_layout() {
	local architecture="$1" tag="$2"
	local oci="${work}/oci" diff_ids="" layers=""
	mkdir -p "${oci}/blobs/sha256"

	local i
	for i in $(seq 1 "${SEED_LAYER_COUNT}"); do
		local rootfs="${work}/rootfs-${i}"
		local layer_tar="${work}/layer-${i}.tar"
		local layer_gz="${work}/layer-${i}.tar.gz"
		write_layer_rootfs "${rootfs}" "${i}"
		tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
			-C "${rootfs}" -cf "${layer_tar}" .
		gzip -n -c "${layer_tar}" >"${layer_gz}"

		local diff_id digest size
		diff_id="$(digest_file "${layer_tar}")"
		digest="$(digest_file "${layer_gz}")"
		size="$(size_file "${layer_gz}")"
		cp "${layer_gz}" "${oci}/blobs/sha256/${digest}"
		diff_ids="$(append_json_item "${diff_ids}" "\"sha256:${diff_id}\"")"
		layers="$(append_json_item "${layers}" "{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar+gzip\",\"digest\":\"sha256:${digest}\",\"size\":${size}}")"
	done

	local config=""
	if [ -n "${SEED_RUNNABLE}" ]; then
		config='"Cmd":["/hello"]'
	fi
	if [ -n "${SEED_ALTERNATE_CONFIG}" ]; then
		config='"Env":["CFS_E2E_CONFIG_SUBSTITUTION=1"],"Cmd":["/hello"]'
	fi
	cat >"${work}/config.json" <<EOF
{"created":"1970-01-01T00:00:00Z","architecture":"${architecture}","os":"linux","config":{${config}},"rootfs":{"type":"layers","diff_ids":[${diff_ids}]}}
EOF
	local config_digest config_size
	config_digest="$(digest_file "${work}/config.json")"
	config_size="$(size_file "${work}/config.json")"
	cp "${work}/config.json" "${oci}/blobs/sha256/${config_digest}"

	cat >"${work}/manifest.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:${config_digest}","size":${config_size}},"layers":[${layers}]}
EOF
	local manifest_digest manifest_size
	manifest_digest="$(digest_file "${work}/manifest.json")"
	manifest_size="$(size_file "${work}/manifest.json")"
	cp "${work}/manifest.json" "${oci}/blobs/sha256/${manifest_digest}"

	local root_type="application/vnd.oci.image.manifest.v1+json"
	local root_digest="${manifest_digest}" root_size="${manifest_size}"
	if [ -n "${SEED_INDEX_ROOT}" ]; then
		# The tag points at a one-entry image index instead of the bare
		# manifest — the shape a multi-platform registry image has, where a
		# tag resolves to an index digest while the runtime records the
		# platform manifest it picked.
		cat >"${work}/manifest-index.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:${manifest_digest}","size":${manifest_size},"platform":{"architecture":"${architecture}","os":"linux"}}]}
EOF
		root_type="application/vnd.oci.image.index.v1+json"
		root_digest="$(digest_file "${work}/manifest-index.json")"
		root_size="$(size_file "${work}/manifest-index.json")"
		cp "${work}/manifest-index.json" "${oci}/blobs/sha256/${root_digest}"
	fi

	cat >"${oci}/index.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"${root_type}","digest":"sha256:${root_digest}","size":${root_size},"platform":{"architecture":"${architecture}","os":"linux"},"annotations":{"org.opencontainers.image.ref.name":"${tag}"}}]}
EOF
	printf '{"imageLayoutVersion":"1.0.0"}\n' >"${oci}/oci-layout"
	tar -C "${oci}" -cf "${work}/image.oci.tar" .
}

cleanup() {
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
}

main() {
	wait_for_registry
	validate_layer_count
	work="$(mktemp -d)"
	trap cleanup EXIT

	local base_name="${IMAGE_REF%:*}" tag="${IMAGE_REF##*:}"
	write_image_layout "$(detect_architecture)" "${tag}"
	if [ -n "${SEED_KEEP_LAYOUT}" ]; then
		mkdir -p "${SEED_KEEP_LAYOUT}"
		cp -a "${work}/oci/." "${SEED_KEEP_LAYOUT}/"
	fi

	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images import \
		--digests --no-unpack --base-name "${base_name}" "${work}/image.oci.tar"
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images push \
		--plain-http "${IMAGE_REF}"
	echo "seeded ${IMAGE_REF}"
}

main "$@"
