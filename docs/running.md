<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter tracks snapshot metadata and lifecycle (prepare,
> commit, remove) and returns placeholder bind mounts. A pull routed to cfs now
> completes: containerd extracts the layers into per-snapshot directories and
> commits them. The mounts are not yet content-addressed composefs mounts and do
> not compose a snapshot's parent chain, so a pulled image is not yet a usable
> container rootfs; that materialization lands in a later change.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc &
```

The defaults (socket `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock`, root
`/var/lib/containerd-cfs-grpc`) match the config below. See `-help` for flags.

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

Both fetch the image and unpack it through cfs: the snapshotter records the
prepare/commit lifecycle and returns placeholder bind mounts that containerd
extracts the layers into. `ctr snapshots --snapshotter cfs ls` then lists the
committed snapshots, confirming both clients reached cfs through the proxy.

## Quick test without editing config

`ctr image pull --local` skips the transfer service (so the `unpack_config` step
is unnecessary) and still routes to cfs:

```sh
sudo ctr image pull --local --snapshotter cfs docker.io/library/busybox:latest
```
