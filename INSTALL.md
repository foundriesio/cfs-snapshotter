<!-- SPDX-License-Identifier: Apache-2.0 -->

# Installing cfs-snapshotter

Release binaries support Linux amd64 and arm64. Choose an install path,
then connect containerd and check a running container.

| Your system | Install path |
|---|---|
| Debian or Ubuntu based | [1. Debian package](#1-debian-package) |
| Other Linux | [2. Binary archive](#2-binary-archive) |
| Building yourself | [3. From source](#3-from-source) |

Check the [runtime requirements](#runtime-requirements) first. Installing
the binary alone does not configure containerd or Docker.

Download assets from the
[releases page](https://github.com/foundriesio/cfs-snapshotter/releases).
The examples use shell variables: set `ver` to the release number without
`v`, and `arch` to `amd64` or `arm64`. For example:

```sh
ver=0.5.0
arch=amd64
gh release download "v$ver" --repo foundriesio/cfs-snapshotter
sha256sum --check --ignore-missing "cfs-snapshotter_${ver}_checksums.txt"
```

The GitHub CLI is optional; browser downloads work too. For release
candidates, copy the `.deb` filename from the release: its version uses
`.rc.N`, while the archive and checksum filenames use `-rc.N`.

## 1. Debian package

1. Install the downloaded package:

   ```sh
   sudo apt install "./cfs-snapshotter_${ver}_${arch}.deb"
   ```

   It installs `/usr/bin/containerd-cfs-grpc` and enables and starts the
   `cfs-snapshotter` systemd service. The package recommends `containerd`
   and `composefs`; ensure the required tools are actually installed.

2. If needed, set daemon flags in `/etc/default/cfs-snapshotter`:

   ```sh
   CFS_OPTS="-admit=preloaded"
   ```

   This is a line in the file, not a shell command to configure the unit.
   The default is admit-any. See the [mode choice](README.md#choose-how-images-enter-the-store).

3. Continue with [Connect containerd (and Docker)](#connect-containerd-and-docker).
   Missing prerequisites can leave the service failed until configuration
   is complete. Then restart it as shown below.

Upgrade with `sudo apt install ./<new-package>.deb`; roll back with
`sudo apt install --allow-downgrades ./<old-package>.deb`. Uninstall with
`sudo apt remove cfs-snapshotter`.

## 2. Binary archive

1. Extract the archive and install the binary:

   ```sh
   archive="cfs-snapshotter_${ver}_linux_${arch}"
   tar -xzf "$archive.tar.gz"
   sudo install -m 0755 "$archive/containerd-cfs-grpc" /usr/local/bin/
   ```

2. On a systemd host, install the supplied unit. Its packaged path is
   `/usr/bin`, so adjust it before starting the service:

   ```sh
   sed 's|/usr/bin/containerd-cfs-grpc|/usr/local/bin/containerd-cfs-grpc|' \
       "$archive/cfs-snapshotter.service" > cfs-snapshotter.service
   sudo install -m 0644 cfs-snapshotter.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable cfs-snapshotter
   ```

   Optional flags go in `/etc/default/cfs-snapshotter` as in the package
   path. Without systemd, configure your init system to run the daemon,
   or keep `sudo containerd-cfs-grpc` running in another terminal.

3. Continue with [Connect containerd (and Docker)](#connect-containerd-and-docker).
   The archive includes `containerd-cfs.toml` for that step.

Upgrade or roll back by installing the other version's binary and
restarting the service. To uninstall this systemd installation:

```sh
sudo systemctl disable --now cfs-snapshotter
sudo rm /etc/systemd/system/cfs-snapshotter.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/containerd-cfs-grpc
```

## 3. From source

1. Use Go 1.26.6 or newer, as required by `go.mod`. Build from a checkout
   so version stamping can use git:

   ```sh
   git clone https://github.com/foundriesio/cfs-snapshotter.git
   cd cfs-snapshotter
   make build
   sudo make install
   ```

   `make build` writes `bin/containerd-cfs-grpc`. `make install` uses
   `/usr/local/bin`; override `PREFIX` or `DESTDIR` for packaging.

2. On a systemd host, install the unit for that binary path:

   ```sh
   sed 's|/usr/bin/containerd-cfs-grpc|/usr/local/bin/containerd-cfs-grpc|' \
       debian/cfs-snapshotter.service > cfs-snapshotter.service
   sudo install -m 0644 cfs-snapshotter.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable cfs-snapshotter
   ```

   Without systemd, keep `sudo containerd-cfs-grpc` running in another
   terminal.

3. Connect containerd using `docs/containerd-cfs.toml` from the checkout.

The default build and release artifacts use ocicfs's `auto` fs-verity
mode. To require fs-verity:

```sh
make build GOFLAGS=-tags=ocicfs_verity_required
```

An `ocicfs_verity_off` build disables it. `make deb` sets its own
`GOFLAGS` in `debian/rules`, so this override does not select the Debian
package's mode. See [fs-verity modes](#fs-verity-modes).

## Connect containerd (and Docker)

### 1. Register the snapshotter and differ

Use the supplied `containerd-cfs.toml`:

| Installation | Configuration source |
|---|---|
| Debian package | `/usr/share/doc/cfs-snapshotter/containerd-cfs.toml` |
| Binary archive | `$archive/containerd-cfs.toml` |
| Source checkout | `docs/containerd-cfs.toml` |

For the package, copy it to a drop-in directory (substitute the source
path for other installations):

```sh
sudo mkdir -p /etc/containerd/conf.d
sudo cp /usr/share/doc/cfs-snapshotter/containerd-cfs.toml /etc/containerd/conf.d/
```

Add the import to `/etc/containerd/config.toml`. Keep existing imports.
`imports` must be a top-level key, before any `[section]` header. For a
version 2 configuration:

```toml
version = 2
imports = ["/etc/containerd/conf.d/*.toml"]
```

Alternatively, merge the supplied block into the main config by hand;
keep only one top-level `version` key and merge existing plugin tables.
The package does not edit containerd's configuration for you.

The [supplied block](docs/containerd-cfs.toml) registers `cfs` and
`cfs-diff` on `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock`. It also:

- Exports `enable_remote_snapshot_annotations = "true"`, so transfer
  pulls carry the image identity needed for admission.
- Puts `cfs-diff` before `walking` in the diff service for Docker. The cfs
  differ handles its staging mounts and declines other mount types.
- Selects `cfs-diff` in the cfs transfer-service `unpack_config` entry.
  Setting this list replaces the built-in default, so the block includes
  `overlayfs`. Retain entries for any other snapshotters you use.

Both plugins are needed for admit-any pulls and `docker load` in either
mode. A missing or mis-ordered differ makes the layer apply fail with
`mount: no such device`: another differ cannot mount the cfs staging
placeholder.

Restart containerd to load the new configuration, then start or restart
the snapshotter:

```sh
sudo systemctl restart containerd
sudo systemctl restart cfs-snapshotter
```

### 2. Connect Docker, if used

Docker must use the containerd instance configured above. Check its
service command with `systemctl cat docker`: an explicit
`--containerd=/run/containerd/containerd.sock` selects that instance.
For other installations, configure the equivalent daemon setting; Docker
may otherwise start its own containerd. See the
[dockerd reference](https://docs.docker.com/reference/cli/dockerd/).

Merge these settings into `/etc/docker/daemon.json`:

```json
{
  "features": {
    "containerd-snapshotter": true
  },
  "storage-driver": "cfs"
}
```

For preloaded mode on Docker 29.9 or newer, also set `"lazy-pull": true`
in `features`. Without this setting, `docker pull` fetches every layer
even when the store holds the image (observed on Docker 29.9.1).
See [Docker's lazy-pull configuration](https://docs.docker.com/engine/storage/containerd/#configuration).

```sh
sudo systemctl restart docker
```

Changing the storage backend hides existing images and containers until
you switch back; it does not migrate them. See
[Docker's image-store guide](https://docs.docker.com/engine/storage/containerd/).

## Check the installation

### 1. Check the binary and configuration

```sh
containerd-cfs-grpc -version
sudo containerd-cfs-grpc -check
```

A release binary prints `containerd-cfs-grpc v<ver> (<commit>)`. Pass the
same mode and paths to `-check` as to the daemon; it does not read
`CFS_OPTS` itself. A successful check ends with exactly:

```text
check passed
```

The check reports tools, the store's fs-verity mode, effective ingest
workers, containerd version and both proxy registrations. It opens the
store and may create directories and probe files. It does not test kernel
mounts, Docker settings, differ order or transfer-service differ selection.
A missing differ only warns in preloaded mode, although archive loads
still need it. The check also does not validate `-root` or reject a
negative `-ingest-workers` as daemon startup does. An unrecognized
containerd version is not enforced.

Check the actual plugin status too:

```sh
sudo ctr plugins ls | grep cfs
```

Expect `io.containerd.snapshotter.v1` with ID `cfs` and
`io.containerd.differ.v1` with ID `cfs-diff`, both with status `ok`.
Spacing and the differ's platform vary by host. Missing rows mean the
block was not loaded; inspect `sudo containerd config dump` and the
placement of `imports`.

### 2. Check a container

With Docker configured and the daemon in admit-any mode:

```sh
docker pull docker.io/library/busybox:latest
docker run --rm docker.io/library/busybox:latest echo 'it works'
```

Expected container output:

```text
it works
```

For preloaded mode or offline loading, follow [running.md](docs/running.md).

### Service status

```sh
sudo systemctl --no-pager status cfs-snapshotter
```

The status has the form `ok: wired to containerd <version> (proxy plugin
cfs)` or `degraded: ...`. The daemon waits for containerd to answer. Its
first definite verdict rejects an old version or missing required wiring
and exits with status 3; the packaged unit stays failed until you fix it
and restart it. Later drift degrades status without terminating the daemon.
The probe repeats every five minutes, so a containerd fix is noticed
without restarting the snapshotter. A plugin-listing failure is logged
and leaves wiring unverified.

## Runtime requirements

- **Linux kernel:** use 6.6 or newer with EROFS and overlayfs data-only
  lower layers, or a vendor kernel with equivalent support. Stores using
  fs-verity also need its kernel support and overlayfs verity validation.
- **Root:** the daemon mounts filesystems and uses loop devices.
- **Snapshotter root:** `-root` defaults to `/var/lib/containerd-cfs-grpc`.
  Put it on a filesystem suitable for overlay upper/work directories,
  such as ext4 or xfs, not overlayfs. Paths used in overlay options must
  not contain `:` or `,`.
- **containerd:** 2.1 or newer. The daemon rejects recognized older
  versions; transfer pulls need the unpack handshake and image annotations.
- **Docker, if used:** Engine 29 or newer (tested with 29.3.0). Use the
  containerd image store and the shared containerd configured above.
  Docker 29.9 and later also need the [lazy-pull feature](#2-connect-docker-if-used)
  for preloaded mode.
- **`mkcomposefs`:** required on `PATH` for admit-any mode. Obtain it from
  [composefs](https://github.com/composefs/composefs) if your distribution
  does not package it. The daemon refuses admit-any startup without it.
- **ocicfs CLI:** needed to preload images. This snapshotter embeds ocicfs
  v0.5.1; use a compatible CLI and matching store fs-verity mode. Preloaded
  serving needs no build tools, but `ocicfs pull` needs `mkcomposefs` on
  the machine that builds the store.

### fs-verity modes

The store defaults to `/var/lib/ocicfs` (`-ocicfs-root`). The default
`auto` build probes its filesystem: it enables fs-verity when supported
and runs without it otherwise. Other probe errors fail startup. `-check`
reports the result. With fs-verity, the snapshotter adds `verity=require`
to container overlays; without it, backing file contents are not checked
on reads.

A build with `ocicfs_verity_required` refuses a store that cannot enable
fs-verity. ext4 needs the `verity` feature and a 4096-byte block size;
btrfs can also provide fs-verity. The ocicfs guide includes an optional
[loopback ext4 setup](https://github.com/foundriesio/ocicfs/blob/main/docs/mount.md#optional-add-fs-verity-with-a-loop-device).

Keep the CLI and daemon's effective store modes consistent. A daemon
requiring fs-verity rejects an image object written without it; backing
file reads through its overlay fail with EIO if those files lack it. An
`ocicfs_verity_off` build does not enforce fs-verity even on objects that
have it. Before changing modes, stop containers and unmount
all mounts of the store: existing overlays keep their original mode.
An auto probe depends on the filesystem behind the store path. If an
attacker able to alter that path must not disable verification, use a
required build.

## Verifying provenance (optional)

The release workflow produces GitHub build-provenance attestations for
its artifacts. Verify an archive with the GitHub CLI:

```sh
gh attestation verify "cfs-snapshotter_${ver}_linux_${arch}.tar.gz" \
    --repo foundriesio/cfs-snapshotter
```

Each architecture also has an SPDX software bill of materials:
`cfs-snapshotter_<ver>_linux_<arch>.spdx.json`.
