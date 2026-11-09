# cfs-snapshotter

cfs-snapshotter lets Docker and containerd serve container filesystems
from a shared [ocicfs](https://github.com/foundriesio/ocicfs) store. It is
for Linux hosts running containers, with the largest benefits on devices
with slow links and small flash storage. Images remain standard OCI images.

The `containerd-cfs-grpc` daemon provides a containerd proxy snapshotter
and proxy differ. The snapshotter prepares filesystems; the differ feeds
pulled layers into the store. Docker or containerd runs the containers.

## Why cfs-snapshotter?

- **Storage.** Identical file contents share one store object across
  layers, images and releases. Admit-any mode also retains the runtime's
  layer blobs, so allow space for both.
- **Bandwidth.** Preload images with `ocicfs pull --delta` to fetch missing
  files from suitable zstd:chunked layers. The runtime then needs only
  image metadata. Ordinary `docker pull` in admit-any mode downloads
  layers normally.
- **Time.** Containers mount prepared image metadata and shared files.
  Preloading avoids runtime layer unpacking; admit-any mode ingests layers
  as containerd downloads them.
- **Integrity.** overlay2 checks downloaded layer archives, but cannot
  tell whether the unpacked image's files remain complete and unchanged.
  ocicfs checks image metadata before mounting. With fs-verity enabled,
  the kernel also rejects missing or altered backing files when read.

## How it works

ocicfs separates file contents from their names, permissions and ownership.
It stores shared file contents alongside a per-image EROFS filesystem
image containing that metadata. composefs joins them at mount time.

The snapshotter mounts read-only EROFS metadata for an image chain and
shares that mount among its containers. Each container gets one flat
writable overlay, combining the image metadata, shared store objects and
its own writes. The daemon does not fetch file data on demand from a
registry: image content must be in the local store before use.

fs-verity is optional. The default build enables it when the store's
filesystem supports it; otherwise mounts do not verify backing file
contents. A build with `ocicfs_verity_required` refuses such a store.
Checks protect image content, not container writes or publisher identity.
See [runtime requirements](INSTALL.md#runtime-requirements) and
[admission and trust](docs/running.md#image-reference-stamping-and-admission).

## Choose how images enter the store

| You want | Daemon mode | Image workflow |
|---|---|---|
| Ordinary Docker/containerd pulls and archive loads | `-admit=any` (default) | `docker pull` or `docker load` fills the store through the differ. |
| An update agent to preload images, including delta pulls | `-admit=preloaded` | `ocicfs pull` first, then a runtime pull of the same image. Unstored images are declined at pull admission. |

Both modes serve the same filesystem layout. Preloaded admission is not a
runtime allowlist: cached images and archive loads need separate policy
for which image configs may run.

## Quick start

1. [Install the daemon](INSTALL.md), connect containerd and, if used,
   Docker. The guide covers the kernel, tools and storage requirements.
2. With the daemon running in the default admit-any mode, pull and run:

   ```sh
   docker pull docker.io/library/busybox:latest
   docker run --rm docker.io/library/busybox:latest echo 'it works'
   ```

   The container prints:

   ```text
   it works
   ```

3. Inspect Docker's snapshots in containerd's `moby` namespace:

   ```sh
   sudo ctr --namespace moby snapshots --snapshotter cfs ls
   ```

For a local test stack instead of host installation, use the
[devcontainer walkthrough](docs/devcontainer.md).

## Documentation

- [Install, configure and check the host](INSTALL.md).
- [Run, preload and load images offline](docs/running.md).
- [Develop, test and inspect mounts](docs/devcontainer.md).
- [Build from source](INSTALL.md#3-from-source).
- [Release procedure](RELEASING.md).

cfs-snapshotter provides storage primitives. Version selection,
activation, rollback and update orchestration belong to the runtime or
update agent.

## Contributing

Please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). All commits must be signed off under
the [Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`), and pull requests must be opened and reviewed by a human.
Automated coding agents must also follow [AGENTS.md](AGENTS.md).

## License

cfs-snapshotter is licensed under the [Apache License 2.0](LICENSE).

Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
