<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter serves real image content from a local
> [knarr](https://github.com/foundriesio/knarr) store. It answers the
> unpacker's per-layer handshake only when the runtime-stamped image
> reference resolves to a held image containing the requested chain, so an
> admitted pull routed to cfs downloads no layers (`ctr` and `docker` still
> fetch the image metadata: index, manifest, config, etc.). A container
> snapshot mounts the image's read-only EROFS metadata — once per image,
> shared by all its containers — and gets one flat overlay whose data comes
> from the store's fs-verity-backed objects (`verity=require`). The
> snapshotter never pulls: images enter the store out of band via
> `knarr pull`, pinned by digest.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc -knarr-root /var/lib/knarr &
```

`-knarr-root` points at the knarr store the out-of-band `knarr pull` writes
(default `/var/lib/knarr`). The other defaults (socket
`/run/containerd-cfs-grpc/containerd-cfs-grpc.sock`, root
`/var/lib/containerd-cfs-grpc`) match the config below. See `-help` for flags.

The snapshotter root hosts each container's overlay upper and work
directories, so it must live on a regular filesystem (ext4, xfs, …) — overlay
cannot stack on overlay. The knarr store's filesystem must support fs-verity
(on ext4 that means `mkfs.ext4 -O verity -b 4096`).

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

The `enable_remote_snapshot_annotations = "true"` export is required. Without
it, transfer-service pulls do not stamp the image identity used by the cfs
admission gate and are declined.

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

## 4. Stage, pull, run

Stage the image in the knarr store first (from a registry or, as in the
devcontainer's tests, from an OCI layout directory):

```sh
sudo knarr pull <registry>/<repo>@sha256:<manifest-digest>
```

Then pull and run through cfs, using the same reference you staged:

```sh
# containerd
sudo ctr image pull --snapshotter cfs <registry>/<repo>@sha256:<manifest-digest>

# Docker (storage-driver already selects cfs)
docker pull <registry>/<repo>@sha256:<manifest-digest>
docker run --rm <registry>/<repo>@sha256:<manifest-digest>
```

A tag reference works the same way when the image was staged by that tag
(`knarr pull <registry>/<repo>:<tag>` followed by
`docker pull <registry>/<repo>:<tag>`): knarr resolves the tag to the digest
its most recent pull by that tag recorded.

Both pulls complete through cfs with zero layer downloads — only the image
metadata (index, manifest, config, etc.) is fetched: the snapshotter answers
the unpacker's per-layer handshake so the layer chain is recorded as already
present. `ctr snapshots --snapshotter cfs ls` then lists the committed chain
snapshots, and `docker run` serves the image content from the knarr store.
Pulling an image the store does not hold fails during the pull at the
snapshotter's admission gate, and running an image whose layer chain is not
in the store fails with an error asking you to pull it by digest out of band.

## Image-reference stamping and admission

The handshake uses `containerd.io/snapshot/cri.image-ref`. Supported runtime
paths stamp it as follows:

| Runtime path | Required configuration |
| --- | --- |
| Docker pull | Docker stamps the reference unconditionally. |
| CRI local pull | Set `disable_snapshot_annotations = false`; the Unix default is `true`. |
| CRI transfer-service pull | Keep the mandatory `enable_remote_snapshot_annotations = "true"` proxy-plugin export shown above. |

The stamped value is the reference exactly as invoked, and the snapshotter
resolves it against the knarr store: a digest reference must be held, and a tag
reference resolves to the digest knarr's most recent pull by that tag recorded —
a tag knarr holds no alias for is declined. A missing annotation is also
declined; non-stamping configurations are unsupported for image-identity
admission. If an unsupported client can forge the annotation, chain membership
still limits it to rootfs content belonging to the named held image, but the
snapshotter cannot establish the unheld config's identity.

This pull-time gate is not a runtime allowlist. Starting an image already cached
or pre-loaded in the runtime can bypass the pull handshake, and container-create
carries only the content chain, not the image reference. Runtime policy must
prevent an unheld cached config from using an otherwise held rootfs. Rootfs
digest and fs-verity enforcement remain active independently of this admission
gate.

## Inspecting a running container (devcontainer)

The devcontainer's seeded test image contains a single static `/hello` binary
that prints its greeting and exits, so a plain `docker run` finishes
immediately, leaving nothing to inspect. Run it with the `sleep` argument to
make it block until stopped, keeping the container and its mounts up:

```sh
docker run --rm -d --name cfs-hold <image> /hello sleep
pid="$(docker inspect -f '{{.State.Pid}}' cfs-hold)"
cat "/proc/${pid}/mountinfo"   # or: findmnt -N "${pid}"
docker stop cfs-hold           # clean shutdown, no SIGKILL wait
```

The container's root is one `overlay` mount whose `lowerdir` ends with the
image's EROFS metadata mount followed by `::<knarr objects>` (a data-only
layer) and `verity=require`; the shared EROFS mount itself appears in the
host's mount table under the snapshotter root
(`.../containerd-cfs-grpc/snapshots/<id>/fs`, fstype `erofs`, on a loop
device). The test image has no shell, so inspect the mounts from the host via
the container's PID as above.
