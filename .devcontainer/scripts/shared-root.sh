#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Shared loop-backed ext4 root: constants and mount helpers sourced by
# entrypoint.sh and run-cfs.sh.

KNARR_ROOT=/var/lib/knarr
SHARED_ROOT=/var/lib/cfs-shared-root
SHARED_ROOT_IMG=/var/tmp/cfs-shared-root.img

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
	dd if=/dev/zero of="${img}" bs=1M count=1024 status=none
	mkfs.ext4 -q -F -O verity -b 4096 "${img}"
	dev="$(losetup -f)"
	[ -e "${dev}" ] || mknod "${dev}" b 7 "${dev##*loop}"
	losetup "${dev}" "${img}"
	mount "${dev}" "${dir}"
	echo "shared fs-verity ext4 at ${dir}"
}

# bind_root binds a subdirectory of the shared loop-backed ext4 onto a
# snapshotter root, giving its overlay upper/work dirs a real filesystem: they
# cannot live on the container's own overlayfs (overlay cannot stack on
# overlay). A filesystem already mounted at root is left as is.
bind_root() {
	local sub="$1" root="$2"
	if mountpoint -q "${root}" 2>/dev/null; then
		return 0
	fi
	mkdir -p "${SHARED_ROOT}/${sub}" "${root}"
	mount --bind "${SHARED_ROOT}/${sub}" "${root}"
}
