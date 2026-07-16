<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter is a remote-snapshotter stub. It answers the
> unpacker's per-layer handshake as if the image content were already present
> locally, so a pull routed to cfs downloads no layers (`ctr` and `docker`
> still fetch the image metadata: index, manifest, config, etc.), and it
> serves every snapshot as one flat overlay whose base layer is a stand-in
> rootfs (`-stub-rootfs`) — enough to run containers, against stand-in content.
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

Both complete through cfs with zero layer downloads — only the image
metadata (index, manifest, config, etc.) is fetched: the snapshotter answers the
unpacker's per-layer handshake as if the layer chain were already present
locally. `ctr snapshots --snapshotter cfs ls` then lists the committed chain
snapshots, confirming both clients reached cfs through the proxy, and
`docker run` serves the stand-in rootfs.

## Quick test without editing config

`ctr image pull --local` skips the transfer service (so the `unpack_config` step
is unnecessary) and still routes to cfs:

```sh
sudo ctr image pull --local --snapshotter cfs docker.io/library/busybox:latest
```
