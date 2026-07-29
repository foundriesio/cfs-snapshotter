#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Shared loop-backed ext4 root: constants and mount helpers sourced by
# entrypoint.sh and run-cfs.sh.

KNARR_ROOT="${KNARR_ROOT:-/var/lib/knarr}"
SHARED_ROOT=/var/lib/cfs-shared-root
SHARED_ROOT_IMG=/var/tmp/cfs-shared-root.img
# Size of the backing image in MiB. It takes effect when the filesystem is
# first created; an already-mounted shared root keeps its size.
SHARED_ROOT_SIZE_MB="${CFS_SHARED_ROOT_SIZE_MB:-1024}"

# mount_loop_ext4 mounts a fresh fs-verity-enabled loop-backed ext4 filesystem
# at dir. A filesystem already mounted at dir is left as is. The container's
# /dev is a plain tmpfs, so the loop node the kernel hands out may not exist
# yet; create it before attaching.
mount_loop_ext4() {
	local img="$1" dir="$2"
	if mountpoint -q "${dir}" 2>/dev/null; then
		return 0
	fi
	local stale dev
	stale="$(losetup -a 2>/dev/null | grep -F "${img}" | cut -d: -f1 || true)"
	for dev in ${stale}; do
		losetup -d "${dev}" 2>/dev/null || true
	done
	mkdir -p "${dir}"
	dd if=/dev/zero of="${img}" bs=1M count="${SHARED_ROOT_SIZE_MB}" status=none
	mkfs.ext4 -q -F -O verity -b 4096 "${img}"
	dev="$(losetup -f)"
	[ -e "${dev}" ] || mknod "${dev}" b 7 "${dev##*loop}"
	losetup "${dev}" "${img}"
	mount "${dev}" "${dir}"
	echo "shared fs-verity ext4 at ${dir} (${SHARED_ROOT_SIZE_MB}M)"
}

# bind_root binds a subdirectory of the shared loop-backed ext4 onto a store
# root, giving the knarr store fs-verity and the overlay upper/work dirs a real
# filesystem: they cannot live on the container's own overlayfs (overlay cannot
# stack on overlay). The subdirectory is named after the root's last path
# element, so two roots never share backing state. A filesystem already mounted
# at root is left as is.
bind_root() {
	local root="$1"
	if mountpoint -q "${root}" 2>/dev/null; then
		return 0
	fi
	mkdir -p "${SHARED_ROOT}/${root##*/}" "${root}"
	mount --bind "${SHARED_ROOT}/${root##*/}" "${root}"
}
