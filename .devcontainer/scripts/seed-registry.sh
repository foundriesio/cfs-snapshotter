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
SEED_LAYER_COUNT=${SEED_LAYER_COUNT:-1}
SEED_NONCE=${SEED_NONCE:-}
SEED_KEEP_LAYOUT=${SEED_KEEP_LAYOUT:-}
# When set, layer 1 carries a static /hello binary and the image config runs
# it, so the seeded image is runnable with docker run. The binary writes and
# reads back a file in the container root (proving the rootfs is writable)
# and then prints "cfs hello ${SEED_NONCE}".
SEED_RUNNABLE=${SEED_RUNNABLE:-}

work=""

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

append_json_item() {
	local current="$1"
	local item="$2"

	if [ -n "${current}" ]; then
		printf '%s,%s' "${current}" "${item}"
	else
		printf '%s' "${item}"
	fi
}

validate_seed_layer_count() {
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

write_layer_rootfs() {
	local rootfs="$1"
	local layer="$2"

	mkdir -p "${rootfs}"
	if [ "${layer}" = 1 ] && [ -n "${SEED_RUNNABLE}" ]; then
		local gosrc
		gosrc="$(mktemp -d)"
		cat >"${gosrc}/hello.go" <<EOF
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// "sleep" keeps the container alive until it is stopped; the process runs
	// as PID 1, which ignores unhandled signals, so SIGTERM is handled
	// explicitly for docker stop to succeed without the kill timeout.
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		<-term
		os.Exit(0)
	}
	// "persist" writes a marker file on the first run and verifies it on a
	// later run of the same container, so a test can tell a restarted
	// container (upper kept) from a fresh one (clean upper).
	if len(os.Args) > 1 && os.Args[1] == "persist" {
		if b, err := os.ReadFile("/somefile"); err == nil {
			if string(b) != "test write" {
				os.Stderr.WriteString("unexpected content: " + string(b) + "\n")
				os.Exit(1)
			}
			os.Stdout.WriteString("persist ok\n")
			return
		}
		if err := os.WriteFile("/somefile", []byte("test write"), 0o644); err != nil {
			os.Stderr.WriteString("write: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("persist wrote\n")
		return
	}
	if err := os.WriteFile("/scratch.txt", []byte("rw"), 0o644); err != nil {
		os.Stderr.WriteString("write: " + err.Error() + "\n")
		os.Exit(1)
	}
	b, err := os.ReadFile("/scratch.txt")
	if err != nil || string(b) != "rw" {
		os.Stderr.WriteString("read back failed\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("cfs hello ${SEED_NONCE}\n")
}
EOF
		# -trimpath keeps the temporary build directory out of the binary, so
		# the same nonce reproduces byte-identical layers across seeds (images
		# sharing a nonce share their layer blobs and thus their chain prefix).
		CGO_ENABLED=0 go build -trimpath -o "${rootfs}/hello" "${gosrc}/hello.go"
		rm -rf "${gosrc}" || echo "warning: failed to remove ${gosrc}" >&2
	fi
	if [ "${SEED_LAYER_COUNT}" = 1 ] && [ -z "${SEED_NONCE}" ]; then
		printf 'cfs smoke test\n' >"${rootfs}/hello.txt"
		return
	fi
	printf 'cfs seed layer %s %s' "${layer}" "${SEED_NONCE}" >"${rootfs}/layer-${layer}.txt"
}

write_image_layout() {
	local work="$1"
	local architecture="$2"
	local tag="$3"
	local oci="${work}/oci"
	local diff_ids=""
	local layers=""

	mkdir -p "${oci}/blobs/sha256"

	for i in $(seq 1 "${SEED_LAYER_COUNT}"); do
		local rootfs="${work}/rootfs-${i}"
		local layer_tar="${work}/layer-${i}.tar"
		local layer_gz="${work}/layer-${i}.tar.gz"
		if [ "${SEED_LAYER_COUNT}" = 1 ] && [ -z "${SEED_NONCE}" ]; then
			rootfs="${work}/rootfs"
			layer_tar="${work}/layer.tar"
			layer_gz="${work}/layer.tar.gz"
		fi

		write_layer_rootfs "${rootfs}" "${i}"
		# Normalized tar: identical rootfs content must yield an identical
		# blob regardless of when it was built, so seeds sharing a nonce
		# really share layer digests.
		tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
			-C "${rootfs}" -cf "${layer_tar}" .
		gzip -n -c "${layer_tar}" >"${layer_gz}"

		local layer_diff_id layer_digest layer_size
		layer_diff_id="$(digest_file "${layer_tar}")"
		layer_digest="$(digest_file "${layer_gz}")"
		layer_size="$(size_file "${layer_gz}")"
		cp "${layer_gz}" "${oci}/blobs/sha256/${layer_digest}"

		diff_ids="$(append_json_item "${diff_ids}" "\"sha256:${layer_diff_id}\"")"
		layers="$(append_json_item "${layers}" "{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar+gzip\",\"digest\":\"sha256:${layer_digest}\",\"size\":${layer_size}}")"
	done

	local config=""
	if [ -n "${SEED_RUNNABLE}" ]; then
		config='"Cmd":["/hello"]'
	fi
	cat >"${work}/config.json" <<EOF
{"created":"1970-01-01T00:00:00Z","architecture":"${architecture}","os":"linux","config":{${config}},"rootfs":{"type":"layers","diff_ids":[${diff_ids}]},"history":[{"created":"1970-01-01T00:00:00Z","created_by":"cfs devcontainer smoke image"}]}
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

	cat >"${oci}/index.json" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:${manifest_digest}","size":${manifest_size},"platform":{"architecture":"${architecture}","os":"linux"},"annotations":{"org.opencontainers.image.ref.name":"${tag}"}}]}
EOF
	printf '{"imageLayoutVersion":"1.0.0"}\n' >"${oci}/oci-layout"

	tar -C "${oci}" -cf "${work}/smoke.oci.tar" .
}

cleanup_work() {
	if [ -n "${work}" ] && [ -e "${work}" ]; then
		rm -rf "${work}" || echo "warning: failed to remove ${work}" >&2
	fi
}

main() {
	wait_for_registry
	validate_seed_layer_count

	work="$(mktemp -d)"
	trap cleanup_work EXIT

	local base_name tag
	base_name="${IMAGE_REF%:*}"
	tag="${IMAGE_REF##*:}"

	write_image_layout "${work}" "$(detect_architecture)" "${tag}"

	if [ -n "${SEED_KEEP_LAYOUT}" ]; then
		mkdir -p "${SEED_KEEP_LAYOUT}"
		cp -a "${work}/oci/." "${SEED_KEEP_LAYOUT}/"
		echo "kept OCI layout at ${SEED_KEEP_LAYOUT}"
	fi

	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images import \
		--digests \
		--base-name "${base_name}" \
		"${work}/smoke.oci.tar"
	ctr --address "${CONTAINERD_SOCKET}" --namespace "${SEED_NAMESPACE}" images push \
		--plain-http \
		"${IMAGE_REF}"

	echo "seeded ${IMAGE_REF}"
}

main "$@"
