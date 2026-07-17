#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds a minimal stand-in rootfs for the snapshotter's stub data plane: a single
# static /hello binary that prints and exits, which is enough for
# `docker run --rm <image> /hello` to exercise the whole container lifecycle.
#
# Setting CFS_STUB_HOLD in the container's environment makes it print and then
# block instead of exiting, so the container (and its overlay mount) stays up
# for inspection from the host:
#   docker run --rm -d -e CFS_STUB_HOLD=1 --name cfs-hold <image> /hello
#   pid="$(docker inspect -f '{{.State.Pid}}' cfs-hold)"
#   cat "/proc/${pid}/mountinfo"   # or: findmnt -N "${pid}"
#   docker stop cfs-hold
#
#   .devcontainer/scripts/make-stub-rootfs.sh [dir]   # default /tmp/cfs-stub-rootfs
set -euo pipefail

DIR="${1:-/tmp/cfs-stub-rootfs}"
SRC="$(mktemp -d)"
trap 'rm -rf "${SRC}"' EXIT

cat >"${SRC}/main.go" <<'GO'
package main

import (
	"os"
	"time"
)

func main() {
	os.Stdout.WriteString("hello from the cfs stub rootfs\n")
	// Opt-in: block instead of exiting, so the container stays up for
	// inspection. A plain SIGTERM (docker stop) still kills it -- Go
	// installs no handler for it, so the default disposition applies.
	// (A bare "select {}" would trip the runtime's deadlock detector,
	// since nothing could ever wake it; a timer-based sleep does not.)
	if os.Getenv("CFS_STUB_HOLD") != "" {
		for {
			time.Sleep(time.Hour)
		}
	}
}
GO
cat >"${SRC}/go.mod" <<'GO'
module hello

go 1.26
GO

mkdir -p "${DIR}/bin"
CGO_ENABLED=0 go -C "${SRC}" build -o "${DIR}/hello" .
# Link the common shell paths to the greeter: an image whose default command
# is /bin/sh or /bin/bash then completes under a bare `docker run`, while
# exotic entrypoints still fail visibly (the stand-in is not the image).
ln -sf ../hello "${DIR}/bin/sh"
ln -sf ../hello "${DIR}/bin/bash"
echo "stub rootfs at ${DIR} (docker run --rm <image> /hello; shell defaults link to it)"
