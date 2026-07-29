<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on a
unix socket. containerd — and Docker, which uses the same containerd — is pointed
at it through one `proxy_plugins` entry.

> **Status:** the snapshotter serves real image content from a local
> [knarr](https://github.com/foundriesio/knarr) store. A container
> snapshot mounts the image's read-only EROFS metadata — once per image,
> shared by all its containers — and gets one flat overlay whose data comes
> from the store's fs-verity-backed objects (`verity=require`). By default
> the runtime's own pull fills the store: containerd fetches the layers as
> it always would and the snapshotter builds them into a composefs image;
> see [Pulling without staging first](#pulling-without-staging-first). A
> machine set up the other way (`-allow-unstaged-pulls=false`) never fills
> the store itself: images enter it out of band via `knarr pull`, pinned by
> digest, and the snapshotter answers the unpacker's per-layer handshake
> only when the runtime-stamped image reference resolves to a held image
> containing the requested chain — an admitted pull routed to cfs downloads
> no layers (`ctr` and `docker` still fetch the image metadata: index,
> manifest, config, etc.), and every other pull is declined.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc -knarr-root /var/lib/knarr &
```

The defaults match the config below; see `-help` for the full list.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-address` | `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock` | The snapshotter's own gRPC socket, named again in containerd's `proxy_plugins` entry. |
| `-root` | `/var/lib/containerd-cfs-grpc` | Where the snapshotter keeps its metadata and each snapshot's directories. |
| `-knarr-root` | `/var/lib/knarr` | The knarr store images are served from. |
| `-containerd-address` | `/run/containerd/containerd.sock` | containerd's gRPC socket, whose content store the layers of a staged pull are read back from. |
| `-allow-unstaged-pulls` | `true` | Selects the machine's work mode: stage every pull through containerd's content store and build it into the knarr store, instead of admitting only images already there. `=false` selects the out-of-band mode. |
| `-log-level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`. |

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

This flow is the out-of-band mode: it needs the daemon started with
`-allow-unstaged-pulls=false` (with the staged default, a pull fetches its
layers instead; see
[Pulling without staging first](#pulling-without-staging-first)).

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

## Pulling without staging first

By default the store fills from the runtime's own pull instead of from an
out-of-band one — no `knarr pull` first:

```sh
sudo ./bin/containerd-cfs-grpc -knarr-root /var/lib/knarr &

docker pull <registry>/<repo>@sha256:<manifest-digest>
docker run --rm <registry>/<repo>@sha256:<manifest-digest>
```

Nothing else changes: the pull is a plain `docker pull`, with its usual
per-layer progress, and the container gets the same mounts an out-of-band pull
would have given it. What differs is where the content comes from. containerd
fetches the layers it always would; the snapshotter stages each one into the
snapshot the unpacker prepared for it, and when the chain's last layer is
committed it builds the whole image into the knarr store — reading the layers
back out of containerd's content store, so nothing is fetched twice — and
discards the staged copies. From then on a container mounts the image's
composefs like any other. A later pull of the same reference, after the
runtime dropped it, fetches the layers again like any docker pull would — on a
staging machine the runtime's content store keeps the layers of everything it
lists, which is what lets a pull that extends an already-present image read
the shared layers back instead of failing over their absence — and the
rebuilt image converges on the same single composefs image in the store.

The image is pinned in the store for as long as the runtime lists it. A
`knarr remove` of an image a runtime is holding declines and names the holder;
`docker rmi`, once containerd has forgotten the image's snapshots, drops the
pin.

The two work modes are exclusive, and the flag defaults to on:

- **Out of band (`-allow-unstaged-pulls=false`).** Only images already in the
  store are admitted; every other pull is declined at the snapshotter's
  gate. What runs on the machine is decided by whatever put the images
  there — an update agent pulling digests it verified — and not by whoever
  can reach the runtime.
- **Staged (default).** Any pull the runtime accepts fills the store, and
  every pull is staged — even of an image the store already holds.
  The content is still verified as it is built, mounted under fs-verity, and
  identical on disk to an out-of-band pull; what staged mode gives up is the
  gate deciding *which* images may arrive, and the out-of-band mode's
  zero-download answer to a pull of a store-held image.

Pick one per machine and keep it: a store filled one way and then served the
other has records made against a posture that no longer holds.

## Image-reference stamping and admission

The handshake uses `containerd.io/snapshot/cri.image-ref`. Supported runtime
paths stamp it as follows:

| Runtime path | Required configuration |
| --- | --- |
| Docker pull | Docker stamps the reference unconditionally. |
| CRI local pull | Set `disable_snapshot_annotations = false`; the Unix default is `true`. |
| CRI transfer-service pull | Keep the mandatory `enable_remote_snapshot_annotations = "true"` proxy-plugin export shown above. |

The stamped value is the reference exactly as invoked. In the out-of-band
mode the snapshotter resolves it against the knarr store: a digest reference
must be held, and a tag reference resolves to the digest knarr's most recent
pull by that tag recorded — a tag knarr holds no alias for is declined. A
staging machine stages the pull without consulting the store, but the
reference and manifest digest still anchor the conversion. A missing
annotation is declined in both modes; non-stamping configurations are
unsupported for image-identity
admission. Staged mode turns the two declines that mean "the store
does not hold this" into admissions that stage the pull; a reference the
runtime does not stamp at all is still declined, in either mode. If an unsupported client can forge the annotation, chain membership
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
