#!/bin/sh
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Build the cfs-snapshotter .deb inside the cfs-snapshotter-debuild container
# (Makefile deb target). Expects the source tree mounted read-only at /src, a
# vendored module tree at /vendor (the knarr dependency is not fetchable from
# inside this container, so the build runs fully offline), a writable output
# directory at /out, and VERSION, REVISION and DEB_VERSION in the environment:
# the Makefile's git-derived fallbacks would silently stamp a "dev" binary
# here, so the caller must pass the real values.
set -eu

: "${VERSION:?}" "${REVISION:?}" "${DEB_VERSION:?}"

cp -a /src /build/cfs-snapshotter
cp -a /vendor /build/cfs-snapshotter/vendor
cd /build/cfs-snapshotter

cat > debian/changelog <<EOF
cfs-snapshotter (${DEB_VERSION}) unstable; urgency=medium

  * Automated build of cfs-snapshotter ${VERSION}.
  * Source revision: ${REVISION}
  * Release notes: https://github.com/foundriesio/cfs-snapshotter/releases

 -- Mykhaylo Sul <mykhaylo.sul@oss.qualcomm.com>  $(date -R -u)
EOF

# PATH must survive debuild's environment sanitizing: the Go toolchain
# lives in /usr/local/go/bin here, which debuild's default PATH lacks.
debuild --preserve-envvar=VERSION --preserve-envvar=REVISION \
        --preserve-envvar=PATH \
        --no-lintian -b -us -uc

mv /build/cfs-snapshotter_*.deb /out/
mv /build/cfs-snapshotter_*.changes /out/
mv /build/cfs-snapshotter_*.buildinfo /out/ 2>/dev/null || true
