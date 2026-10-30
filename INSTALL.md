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
systemd service, enabled and started. The daemon refuses to run until
its prerequisites are met, and the unit shows as **failed** until they
are: the staged-mode store tools immediately (most visibly
`mkcomposefs`, since the composefs package is not yet in the Debian or
Ubuntu archives), and the containerd side — version 2.1+ and the
[wiring below](#connect-containerd-and-docker) — as soon as containerd
is up to answer. Once the
[runtime requirements](#runtime-requirements) and the wiring are in
place, `sudo systemctl restart cfs-snapshotter` recovers the unit.
Daemon flags (for example the out-of-band mode,
`-allow-unstaged-pulls=false`, which needs no local build tools) go in
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
configuration block used below) and the `cfs-snapshotter` systemd unit
as a ready template:

```sh
sudo install -m 0644 cfs-snapshotter_<ver>_linux_<arch>/cfs-snapshotter.service \
    /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cfs-snapshotter
```

(The unit starts `/usr/bin/containerd-cfs-grpc`; adjust `ExecStart` if
you installed the binary under `/usr/local/bin`.) Without systemd,
start the daemon from your init system or by hand:

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

- Keep the block in its own file: copy it to a drop-in directory and make
  sure the main config imports that directory. `imports` is a top-level
  TOML key — it must sit **above any `[section]` header** in
  `config.toml`; appended at the end of the file it lands inside the last
  section and is silently ignored.

  ```sh
  sudo mkdir -p /etc/containerd/conf.d
  sudo cp /usr/share/doc/cfs-snapshotter/containerd-cfs.toml /etc/containerd/conf.d/
  ```

  ```toml
  # first lines of /etc/containerd/config.toml, before any [section]
  version = 2
  imports = ["/etc/containerd/conf.d/*.toml"]
  ```

- Or append the block to the main config — and if `config.toml` already
  has a `version` line, delete the duplicate from the appended block:

  ```sh
  sudo tee -a /etc/containerd/config.toml < /usr/share/doc/cfs-snapshotter/containerd-cfs.toml
  ```

After the restart, containerd must list both plugins — the snapshotter
and the differ, which the same daemon serves on the same socket:

```sh
sudo ctr plugins ls | grep cfs
# io.containerd.snapshotter.v1  cfs       -            ok
# io.containerd.differ.v1       cfs-diff  linux/amd64  ok
```

No `cfs` row means containerd did not read the block — recheck the
`imports` placement above, or `sudo containerd config dump | grep -A3
proxy_plugins` to see what it actually merged. A `cfs` row without a
`cfs-diff` row means an older version of the block is in place: staged
pulls, and `docker load` in either mode, fail to unpack until the differ is
wired in.

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

# Register the cfs differ, served by the same daemon on the same socket.
[proxy_plugins.cfs-diff]
  type = "diff"
  address = "/run/containerd-cfs-grpc/containerd-cfs-grpc.sock"

# Docker applies layers through the diff service: cfs-diff first, which
# takes only the cfs snapshotter's layers, then walking for everything else.
[plugins."io.containerd.service.v1.diff-service"]
  default = ["cfs-diff", "walking"]

# `ctr pull` unpacks via the transfer service, which only unpacks for the
# snapshotters listed here. Listing any entry replaces the built-in default,
# so keep overlayfs alongside cfs.
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "overlayfs"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "cfs"
  differ = "cfs-diff"
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

prints `containerd-cfs-grpc v<ver> (<commit>)` for a release build. One
command verifies the whole setup — the staged-mode prerequisites and the
containerd wiring — and exits non-zero on any failure:

```sh
sudo containerd-cfs-grpc -check
```

Then, with the service running and containerd connected (default staged
mode):

```sh
sudo systemctl --no-pager status cfs-snapshotter
docker pull alpine
docker run --rm alpine echo it works
```

The service's `Status:` line carries the daemon's own verdict on the
containerd side, re-checked every few minutes — `ok: wired to
containerd v2.2.3 (proxy plugin cfs)` when everything matches, or a
`degraded: ...` line naming what to fix. A containerd problem already
present when the daemon starts puts the unit in the **failed** state
instead (fix it, then `sudo systemctl restart cfs-snapshotter`); drift
that appears *after* a healthy start only degrades the status, and
there fixing containerd and restarting *containerd alone* is enough —
the status flips back to ok by itself.

The pull's layers are ingested into the ocicfs store by the snapshotter's
differ as containerd fetches them, and the container's root filesystem
is served from the store as a composefs mount. For offline use, see
[Loading images offline](docs/running.md#loading-images-offline-docker-load).

## Runtime requirements

Installation works on any Linux; *running* the snapshotter needs:

- **Kernel: Linux 6.6 or newer** — EROFS, fs-verity, and overlayfs with
  data-only lower layers and verity validation. A vendor kernel carrying
  the same support also works.
- **fs-verity enabled on the filesystem holding the ocicfs store**
  (`-ocicfs-root`, `/var/lib/ocicfs` by default). ext4 needs a 4096-byte
  block size and the `verity` feature; btrfs supports fs-verity out of
  the box.
- **containerd 2.1 or newer** — the transfer service forwards the unpack
  handshake and image-identity annotations to proxy snapshotters starting
  with containerd 2.1. On anything older (distro archives often still
  ship 1.7) every pull fails with `snapshot has no image in its ancestry;
  scratch snapshots are not supported`. The .deb declares containerd as a
  Recommends so an existing installation from any source is accepted —
  check yours with `containerd --version`.
- **Docker Engine 29 or newer, if Docker is used** — Docker reaches the
  snapshotter through its containerd image store, selected by
  `storage-driver` (tested with Docker 29.3).
- **`mkcomposefs`** (from the
  [composefs](https://github.com/composefs/composefs) project) — needed by
  the default staged-pull mode, which builds pulled images into the store;
  the daemon refuses to start in that mode without it. The .deb declares
  the `composefs` package as a Recommends because it is not yet in the
  Debian-stable or Ubuntu archives.
- **[ocicfs](https://github.com/foundriesio/ocicfs)** — only for the
  out-of-band mode (`-allow-unstaged-pulls=false`), where images enter the
  store via `ocicfs pull` before the runtime asks for them.
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
