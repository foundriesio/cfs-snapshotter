<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter is a remote-snapshotter stub backed by a local
> knarr image store. It answers the unpacker's per-layer handshake for chains
> present in that store, so a pull of an image staged out of band downloads
> no layers (`ctr` and `docker` still fetch the image metadata: index,
> manifest, config, etc.); chains the store does not cover fail closed — the
> snapshotter never fetches or unpacks content itself. Every snapshot is
> served as one flat overlay whose base layer is a stand-in rootfs
> (`-stub-rootfs`) — enough to run containers, against stand-in content.
> Content-addressed composefs mounts of real image content land in a later
> change.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc -stub-rootfs <dir> &
```

`-stub-rootfs` is required: a read-only directory served as every container's
base layer in place of real image content (the devcontainer builds one — see
`.devcontainer/scripts/make-stub-rootfs.sh`). The other defaults (socket
`/run/containerd-cfs-grpc/containerd-cfs-grpc.sock`, root
`/var/lib/containerd-cfs-grpc`, knarr root `/var/lib/knarr`) match the config
below. See `-help` for flags.

Set `-knarr-root` to the shared knarr image store root. Images must be pulled
into that store by digest out of band before containerd prepares their layer
chains through cfs. If a chain digest is not present in the local index,
`Prepare` returns a failed-precondition error naming the chainID and the
required out-of-band pull.

## 2. Configure containerd

Put this in `/etc/containerd/config.toml`, then `sudo systemctl restart containerd`:

```toml
version = 2

# Register the cfs proxy snapshotter (address must match the daemon above).
[proxy_plugins.cfs]
  type = "snapshot"
  address = "/run/containerd-cfs-grpc/containerd-cfs-grpc.sock"

[proxy_plugins.cfs.exports]
  enable_remote_snapshot_annotations = "true"

# `ctr pull` unpacks via the transfer service, which only unpacks for the
# snapshotters listed here. Listing any entry replaces the built-in default,
# so keep overlayfs alongside cfs.
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "overlayfs"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "cfs"
```

## 3. Configure Docker

Docker shares the system containerd, so the entry above is all it needs. Enable
the containerd image store and select cfs in `/etc/docker/daemon.json`, then
`sudo systemctl restart docker`:

```json
{
  "features": {
    "containerd-snapshotter": true
  },
  "storage-driver": "cfs"
}
```

## 4. Pull

```sh
# containerd
sudo ctr image pull --snapshotter cfs docker.io/library/busybox:latest

# Docker (storage-driver already selects cfs)
docker pull busybox
```

When the image is already present in the knarr store, both complete through
cfs with zero layer downloads — only the image metadata (index, manifest,
config, etc.) is fetched: the snapshotter answers the unpacker's per-layer
handshake from the store's index. `ctr snapshots --snapshotter cfs ls` then
lists the committed chain snapshots, confirming both clients reached cfs
through the proxy, and `docker run` serves the stand-in rootfs. An image
absent from the store fails closed rather than silently unpacking into a
different rootfs shape.

## Quick test without editing config

`ctr image pull --local` skips the transfer service (so the `unpack_config` step
is unnecessary) and still routes to cfs:

```sh
sudo ctr image pull --local --snapshotter cfs docker.io/library/busybox:latest
```

## Inspecting a running container (devcontainer stub)

The devcontainer's stand-in rootfs is a single static `/hello` binary — also
linked as `/bin/sh` and `/bin/bash` — that prints a line and exits, so a plain
`docker run` finishes immediately, leaving nothing to inspect. Setting
`CFS_STUB_HOLD` in the container's environment makes it print and then block,
keeping the container and its overlay mount up:

```sh
docker run --rm -d -e CFS_STUB_HOLD=1 --name cfs-hold <image> /hello
pid="$(docker inspect -f '{{.State.Pid}}' cfs-hold)"
cat "/proc/${pid}/mountinfo"   # or: findmnt -N "${pid}"
docker stop cfs-hold           # clean shutdown, no SIGKILL wait
```

The stub rootfs has no real shell, so `docker exec cfs-hold sh` just runs the
same greeter again rather than giving an interactive session — inspect the
mount from the host via the container's PID as above.
