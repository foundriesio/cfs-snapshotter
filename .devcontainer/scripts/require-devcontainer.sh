#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

require_cfs_devcontainer() {
	if [ "${CFS_DEVCONTAINER:-}" = "1" ] && [ -f /opt/cfs-devcontainer/.runtime-marker ]; then
		export PATH="/opt/cfs-dev/bin:${PATH}"
		return 0
	fi

	cat >&2 <<'EOF'
this script must run inside the cfs-snapshotter devcontainer.

Use one of:
  ./devshell.sh <command>
  devcontainer exec --workspace-folder . <command>
  an interactive shell opened by the repository devcontainer
EOF
	exit 1
}
