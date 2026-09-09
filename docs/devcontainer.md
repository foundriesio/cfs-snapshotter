<!-- SPDX-License-Identifier: Apache-2.0 -->

# Devcontainer Runtime Testing

This repository includes a privileged devcontainer for testing the cfs proxy
snapshotter against a nested, version-pinned runtime stack:

- Docker Engine from Docker's static binary archive.
- Upstream containerd from containerd release assets.
- Upstream runc.
- CNI plugins.
- Zot, used as a local test registry on `127.0.0.1:5000`.
- erofs-utils built from source (Debian's package predates the tar mode that
  containerd's built-in erofs snapshotter needs).
- composefs built from source (`mkcomposefs`, `mount.composefs`), which knarr
  shells out to.
- The knarr CLI for staging images into the store — see the note below.
- The locally built `bin/containerd-cfs-grpc`.

The knarr CLI is not baked into the image: the container has no credentials
for the internal knarr module, so `./devshell.sh` builds it on the host at the
exact version the snapshotter pins and mounts the binary at
`/opt/cfs-dev/bin/knarr`. A container opened directly from an editor does not
get it, and `make test-e2e` (which needs it) will fail there — use
`./devshell.sh` for the e2e.

The container is intentionally for development and conformance-style wiring
checks only. It does not add Docker or container lifecycle behavior to
cfs-snapshotter itself.

## Host Requirements

- Linux Docker host.
- Privileged containers enabled.
- Kernel support for nested mounts and the snapshotter operations being tested.
- Network access during devcontainer build to fetch the requested binary
  release archives.

The devcontainer uses Debian Bookworm through
`mcr.microsoft.com/devcontainers/go:<GO_VERSION>-bookworm`. It does not install
Docker or containerd from Debian packages, so runtime versions are controlled
only through build arguments.

## Version Knobs

Override versions in `.devcontainer/devcontainer.json` by changing the build
arguments:

```jsonc
"build": {
  "dockerfile": "Dockerfile",
  "args": {
    "GO_VERSION": "1.26",
    "DOCKER_VERSION": "29.3.0",
    "CONTAINERD_VERSION": "2.2.2",
    "RUNC_VERSION": "1.3.3",
    "CNI_PLUGINS_VERSION": "1.8.0",
    "ZOT_VERSION": "2.1.17",
    "EROFS_UTILS_VERSION": "1.9.2",
    "GOLANGCI_LINT_VERSION": "2.6.2"
  }
}
```

The Dockerfile downloads exact release artifacts and fails during build if a
requested archive is missing or cannot be installed. The composefs build is
pinned by tag and commit through the Dockerfile's `COMPOSEFS_VERSION` and
`COMPOSEFS_COMMIT` build arguments.

Reference release locations:

- Docker static binaries: <https://docs.docker.com/engine/install/binaries/>
- containerd release assets: <https://github.com/containerd/containerd/releases>
- Zot release assets: <https://github.com/project-zot/zot/releases>
- erofs-utils release tags: <https://github.com/erofs/erofs-utils/tags>

## Startup

Opening the devcontainer runs the image entrypoint. The entrypoint:

- Starts Zot, `containerd`, and `dockerd`.
- Mounts one loop-backed, fs-verity-enabled ext4 filesystem and binds
  subdirectories of it as the knarr store root (`/var/lib/knarr`) and the
  snapshotter roots: the knarr store needs fs-verity, and the overlay
  upper/work directories need a real filesystem (they cannot live on the
  container's own overlayfs). Its size in MiB comes from the host's
  `CFS_SHARED_ROOT_SIZE_MB` (default 1024), passed through by `devshell.sh`
  and `devcontainer.json` and applied when the filesystem is first created.
- Starts `bin/containerd-cfs-grpc` with `-knarr-root /var/lib/knarr`, before
  `containerd`, so containerd can register the `cfs` proxy snapshotter during
  startup.
- Waits for `ctr version`, `docker version`, and `docker info`.
- Runs the requested command, or the default `sleep infinity` command for an
  interactive devcontainer session.

The Dockerfile bakes in `/etc/containerd/config.toml` with the `cfs` proxy
snapshotter and unpack entries for `overlayfs` and `cfs`. It also bakes in
`/etc/docker/daemon.json` with Docker's containerd image store enabled and `cfs`
as the selected storage driver. Both Docker and containerd are configured to use
the local insecure registry at `127.0.0.1:5000`.

Logs and pid files are written under `/var/log/cfs-devcontainer/`.

Rebuild and restart the cfs proxy snapshotter after editing snapshotter code:

```sh
make run-cfs
```

`make run-cfs` builds `bin/containerd-cfs-grpc`, restarts it in the background,
and ensures containerd sees the `cfs` proxy snapshotter. It does not restart
containerd after every cfs restart; it restarts containerd only if the `cfs`
proxy plugin is not registered.

The cfs snapshotter runs with trace logging by default in the devcontainer. To
override it:

```sh
CFS_LOG_LEVEL=debug make run-cfs
```

The devcontainer boots the snapshotter in the default staged mode. A plain
`make run-cfs` restarts it that way; `CFS_ALLOW_UNSTAGED_PULLS=0 make run-cfs`
restarts it in the out-of-band mode instead (the mode is part of the state the
restart reconciles, so switching back is another `make run-cfs`).

## End-To-End Test

From the host, run the e2e inside the devcontainer with the nested daemon
stack started:

```sh
./devshell.sh make test-e2e
```

From an already-open devcontainer session (started via `./devshell.sh`, which
provides the knarr CLI):

```sh
make test-e2e
```

`make test-e2e` depends on `make run-cfs`, so it rebuilds and restarts the cfs
proxy first, then runs `.devcontainer/scripts/e2e-fastpath.sh`. The e2e seeds
Zot with runnable multi-layer OCI images
(`.devcontainer/scripts/seed-registry.sh`) and drives the full data plane
through Docker, in two phases — one per work mode, since a machine runs in one
or the other and never both.

The warm phase restarts the snapshotter in the out-of-band mode
(`-allow-unstaged-pulls=false`), admitting only images staged in the knarr
store with `knarr pull --from-dir` pinned by manifest digest:

- Pulls of an image the store does not hold — including one that reuses a held
  image's layers under a different config — are declined at the snapshotter's
  admission gate.
- `docker pull` completes with zero layer fetches and `docker run` prints the
  seeded program's output — real image content, served from the knarr store.
- An admitted image is held in the store: `knarr remove` declines and names
  the holder, the container still runs, and the hold is released once the
  image is removed from the runtime.
- Concurrent containers share one EROFS image mount and each get their own
  flat overlay rootfs.
- An existing container recovers after the shared image mount is unmounted
  and the snapshotter is restarted.
- Removing the image tears down the chain mount and its loop device.
- Tampering with a backing object in the store makes the container fail with
  an I/O error — fs-verity rejects the modified content when overlayfs opens
  the file.

The cold phase then restarts the snapshotter in the staged mode — the boot
default — with
stores of its own (`/var/lib/knarr-cold`,
`/var/lib/containerd-cfs-grpc-cold`), and asserts that a plain `docker pull`
of an image nobody staged:

- lands in the knarr store as exactly one composefs image, present in the CAS
  and named in the fs-verity view, with no staging directories on disk and
  the chain's snapshot records kept;
- reaches the store a layer at a time through the differ: a known file of the
  first layer is a store object while the last, large layer is still being
  transferred;
- runs under `docker run`, on a shared EROFS mount and a `verity=require`
  overlay, and prints the seeded program's output;
- fetches its layers again when it is removed from the runtime and
  re-pulled — a staging machine refetches rather than answering from the
  store — and converges on the same single store image;
- is held in the store until the runtime forgets it, exactly as an
  out-of-band pull is;
- shares its objects with a second image built on the same layers — the store
  does not grow by a second copy;
- leaves nothing behind when the pull is interrupted midway: the active
  snapshot is aborted, the staged records are collected once nothing
  references them, `knarr gc` runs against the store the abandoned build let
  go of, and a re-pull of the same reference converges on the same one
  composefs image;
- builds the same composefs image at one and at two ingest workers, and the
  same one `knarr pull` builds from the image's OCI layout into a fresh store.

The cold phase runs the snapshotter with `-ingest-workers 1` and switches to
`-ingest-workers 2` for the last leg. The e2e prints one line per layer the
differ applied for a cold pull, from the elapsed time the snapshotter logs for
each: `apply: <image> layer <i> in <T>s` for the layers fed into the image
build, and `finish: <image> layer <i> in <T>s` for the last one, which also
finishes the image.

## Conformance Suite

`make root-test` runs containerd's reusable snapshotter conformance suite
(`core/snapshots/testsuite`) against a cfs snapshotter backed by a real knarr
store. It mounts, so it needs root; a plain `make test` skips it.

```sh
./devshell.sh make root-test
```

All 21 subtests currently fail, every one of them on the same call and with
the same error:

```
snapshot has no image in its ancestry; scratch snapshots are not supported
```

The suite builds its fixtures from the bottom up: each test starts by
preparing a snapshot with no parent and applying files into it. cfs has no
answer for that by design — a snapshot's lowest layer is an image in the knarr
store, and a mount with no image beneath it would be unverified scratch, so
`Prepare` refuses it. Nothing gets far enough to exercise what the suite is
really checking (layer semantics, whiteouts, `Rename`, the 128-layer mount),
so the run says nothing about those yet.

Making the suite meaningful needs fixtures that start from an image the store
holds, which the suite has no seam for; the e2e above is what covers the
snapshotter's behaviour in the meantime.

## Debugging Niceties

Two shell functions and an environment variable make interactive debugging
easier from any login shell inside the devcontainer (`./devshell.sh`, a VS Code
terminal, etc.):

- `cfs-log` -- pretty-print the whole `containerd-cfs-grpc` daemon log.
- `cfs-log-follow` -- tail and pretty-print the log as new entries arrive.

Both read `/var/log/cfs-devcontainer/containerd-cfs-grpc.log` (see Startup
above) through `jq`.

A plain `docker run` of the seeded test image exits immediately once it
prints its greeting, leaving nothing to inspect. Running its `/hello` binary
with the `sleep` argument makes it block instead, so the container and its
overlay mount stay up for inspection from the host. See
[docs/running.md](running.md#inspecting-a-running-container-devcontainer)
for the full example.

## Comparing Against The erofs Snapshotter

containerd's built-in erofs snapshotter is wired up as a reference point: it
converts each layer to an EROFS blob at unpack time and mounts the result,
which makes it a useful side-by-side comparison for cfs's snapshot and mount
shapes. Ask for it explicitly (the devcontainer default is cfs):

```sh
ctr image pull --snapshotter=erofs docker.io/library/busybox:latest
ctr run --rm --snapshotter=erofs docker.io/library/busybox:latest erofs-demo echo hello
ctr snapshots --snapshotter erofs ls
```

Unlike cfs, these pulls download the layers: the erofs snapshotter is a local
snapshotter, not a remote one, so no unpack handshake is answered and the
transfer service fetches and converts every layer.

This needs EROFS support in the host kernel. The entrypoint enables the erofs
snapshotter only when `/proc/filesystems` lists erofs; on hosts without it
(CI runners included) it prints a notice and leaves erofs out, and cfs is
unaffected either way.

## Shutdown And Cleanup

Stopping the devcontainer stops the nested daemons with the container.

Delete nested daemon state and logs:

```sh
.devcontainer/scripts/clean-runtime-state.sh --force
```

The cleanup script requires `--force` because it removes `/var/lib/docker`,
`/var/lib/containerd`, `/var/lib/containerd-cfs-grpc`,
`/var/lib/cfs-dev-registry`, related `/run` sockets, and
`/var/log/cfs-devcontainer` inside the devcontainer.
