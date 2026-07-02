<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter serves container root filesystems for images
> already present in the local knarr store, indexed by layer chain digest.
> Each image's EROFS metadata is mounted read-only once, at its chain
> snapshot; containers get standard flat overlay mount specs combining their
> writable upper (and any committed intermediate layers, such as docker's
> init layer) with that shared mount and the knarr object directory as a
> verity-enforced data-only lower, and `docker run` executes from it without
> any layer entering containerd's content store (only the image metadata —
> index, manifest, config, etc. — is fetched). Prepares for unknown chains
> fail closed; the snapshotter does not fetch or unpack missing image
> content as a fallback.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc &
```

The defaults (socket `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock`, root
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
through the proxy, and `docker run` executes the image's real content from
the store. An image
absent from the store fails closed rather than silently unpacking into a
different rootfs shape.

## Quick test without editing config

`ctr image pull --local` skips the transfer service (so the `unpack_config` step
is unnecessary) and still routes to cfs:

```sh
sudo ctr image pull --local --snapshotter cfs docker.io/library/busybox:latest
```
