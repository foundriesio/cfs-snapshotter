<!-- SPDX-License-Identifier: Apache-2.0 -->

# Installing cfs-snapshotter

cfs-snapshotter runs on Linux, amd64 and arm64. No Go toolchain is needed
to install a release. Pick the path that matches your system:

| Your system                        | Install path                        |
| ---------------------------------- | ----------------------------------- |
| Debian or Ubuntu based             | [1. Debian package](#1-debian-package) |
| any other Linux                    | [2. Binary archive](#2-binary-archive) |
| building yourself (Go 1.26+)       | [3. From source](#3-from-source)    |

Installing the binary is not enough by itself: the daemon does nothing
until containerd is pointed at it. Whichever path you pick, finish with
[Connect containerd (and Docker)](#connect-containerd-and-docker).

Release artifacts live on the
[releases page](https://github.com/foundriesio/cfs-snapshotter/releases).
In the commands below, `<ver>` is the release version (e.g. `0.1.0`) and
`<arch>` is `amd64` or `arm64`. Instead of downloading assets one by one
from the browser, the [GitHub CLI](https://cli.github.com/) fetches a
whole release in one step:

```sh
gh release download v<ver> --repo foundriesio/cfs-snapshotter
```

## 1. Debian package

Download `cfs-snapshotter_<ver>_<arch>.deb` and
`cfs-snapshotter_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing cfs-snapshotter_<ver>_checksums.txt
sudo apt install ./cfs-snapshotter_<ver>_<arch>.deb
```

This installs `/usr/bin/containerd-cfs-grpc` and a `cfs-snapshotter`
systemd service, enabled and started. Daemon flags (for example the
out-of-band mode, `-allow-unstaged-pulls=false`) go in
`/etc/default/cfs-snapshotter`, followed by
`sudo systemctl restart cfs-snapshotter`.

- **Upgrade:** `sudo apt install ./cfs-snapshotter_<newver>_<arch>.deb`
- **Rollback:** `sudo apt install --allow-downgrades ./cfs-snapshotter_<oldver>_<arch>.deb`
- **Uninstall:** `sudo apt remove cfs-snapshotter`

## 2. Binary archive

Download `cfs-snapshotter_<ver>_linux_<arch>.tar.gz` and
`cfs-snapshotter_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing cfs-snapshotter_<ver>_checksums.txt
tar -xzf cfs-snapshotter_<ver>_linux_<arch>.tar.gz
sudo install -m 0755 cfs-snapshotter_<ver>_linux_<arch>/containerd-cfs-grpc \
    /usr/local/bin/containerd-cfs-grpc
```

The archive also carries `containerd-cfs.toml` (the containerd
configuration block used below) but no systemd unit: start the daemon
from your init system (the packaged unit,
[debian/cfs-snapshotter.service](debian/cfs-snapshotter.service), is a
ready template) or by hand:

```sh
sudo containerd-cfs-grpc &
```

- **Upgrade / rollback:** install the other version's binary the same way.
- **Uninstall:** `sudo rm /usr/local/bin/containerd-cfs-grpc`

## 3. From source

Needs Go 1.26 or newer. Build from a git checkout, not a source tarball,
so the binary reports its real version:

```sh
git clone https://github.com/foundriesio/cfs-snapshotter.git
cd cfs-snapshotter
make build
sudo make install
```

`make install` places the binary under `/usr/local/bin`; override with
`PREFIX=/usr` or `DESTDIR=<staging>` for packaging. Start the daemon as in
the archive path above.

## Connect containerd (and Docker)

containerd reaches the daemon through one `proxy_plugins` entry. The
required block ships with the package as `containerd-cfs.toml` — at
`/usr/share/doc/cfs-snapshotter/containerd-cfs.toml` from the .deb, at
the top level of the binary archive, and as
[docs/containerd-cfs.toml](docs/containerd-cfs.toml) in a checkout.
Apply it one of two ways, then `sudo systemctl restart containerd`:

- If your `config.toml` declares an `imports` glob (e.g.
  `imports = ["/etc/containerd/conf.d/*.toml"]`), copy the file there:

  ```sh
  sudo cp /usr/share/doc/cfs-snapshotter/containerd-cfs.toml /etc/containerd/conf.d/
  ```

- Otherwise append it to the main config — and if `config.toml` already
  has a `version` line, delete the duplicate from the appended block:

  ```sh
  sudo tee -a /etc/containerd/config.toml < /usr/share/doc/cfs-snapshotter/containerd-cfs.toml
  ```

The package never edits `/etc/containerd/config.toml` itself: the file
belongs to containerd, and the `unpack_config` list below *replaces*
containerd's built-in default (which is why overlayfs is listed
alongside cfs), so merging it is an explicit administrator step. This is
the whole block:

```toml
version = 2

# Register the cfs proxy snapshotter (address must match the daemon).
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

For Docker (which shares the system containerd), also enable the
containerd image store and select cfs in `/etc/docker/daemon.json`, then
`sudo systemctl restart docker`:

```json
{
  "features": {
    "containerd-snapshotter": true
  },
  "storage-driver": "cfs"
}
```

[docs/running.md](docs/running.md) explains every entry, the daemon's
flags, and the two work modes.

## Check the installation

```sh
containerd-cfs-grpc -version
```

prints `containerd-cfs-grpc v<ver> (<commit>)` for a release build. With
the service running and containerd connected (default staged mode):

```sh
sudo systemctl --no-pager status cfs-snapshotter
docker pull alpine
docker run --rm alpine echo it works
```

The pull is staged and built into the knarr store by the snapshotter, and
the container's root filesystem is served from it as a composefs mount.

## Runtime requirements

Installation works on any Linux; *running* the snapshotter needs:

- **Kernel: Linux 6.6 or newer** — EROFS, fs-verity, and overlayfs with
  data-only lower layers and verity validation. A vendor kernel carrying
  the same support also works.
- **A knarr store filesystem with fs-verity enabled.** ext4 needs a
  4096-byte block size and the `verity` feature; btrfs supports fs-verity
  out of the box.
- **containerd** (and optionally Docker on top of it). The .deb declares
  it as a Recommends so an existing installation from any source is
  accepted.
- **`mkcomposefs`** (from the
  [composefs](https://github.com/composefs/composefs) project) — needed by
  the default staged-pull mode, which builds pulled images into the store.
  The .deb declares the `composefs` package as a Recommends because it is
  not yet in the Debian-stable or Ubuntu archives.
- **[knarr](https://github.com/foundriesio/knarr)** — only for the
  out-of-band mode (`-allow-unstaged-pulls=false`), where images enter the
  store via `knarr pull` before the runtime asks for them.
- **Root privileges**: the daemon mounts, uses loop devices, and enables
  fs-verity.

## Verifying provenance (optional)

Beyond checksums, every artifact carries a GitHub build-provenance
attestation — proof it was built by this repository's release workflow
from the tagged commit (needs the [GitHub CLI](https://cli.github.com/)):

```sh
gh attestation verify cfs-snapshotter_<ver>_linux_<arch>.tar.gz \
    --repo foundriesio/cfs-snapshotter
```

An SPDX SBOM per binary (`cfs-snapshotter_<ver>_linux_<arch>.spdx.json`)
is attached to each release.
