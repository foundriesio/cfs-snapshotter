<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

cfs-snapshotter is a containerd **remote (proxy) snapshotter**: a standalone
daemon (`containerd-cfs-grpc`) that serves the containerd Snapshots gRPC API on
a unix socket, and on the same socket the Diff API, through which containerd
hands it the layers of a pull to ingest. containerd — and Docker, which uses
the same containerd — is pointed at them through two `proxy_plugins` entries.

> **Status:** the snapshotter serves real image content from a local
> [ocicfs](https://github.com/foundriesio/ocicfs) store. A container
> snapshot mounts the image's read-only EROFS metadata — once per image,
> shared by all its containers — and gets one flat overlay whose data comes
> from the store's fs-verity-backed objects (`verity=require`). By default
> the runtime's own pull fills the store: containerd fetches the layers as
> it always would and the snapshotter's differ ingests each one into the
> store as it lands, building the composefs image;
> see [Pulling without staging first](#pulling-without-staging-first). A
> machine set up the other way (`-admit=preloaded`) never fills
> the store itself: images enter it out of band via `ocicfs pull`, pinned by
> digest, and the snapshotter answers the unpacker's per-layer handshake
> only when the runtime-stamped image reference resolves to a held image
> containing the requested chain — an admitted pull routed to cfs downloads
> no layers (`ctr` and `docker` still fetch the image metadata: index,
> manifest, config, etc.), and every other pull is declined.

## 1. Build and start the daemon

```sh
make build
sudo ./bin/containerd-cfs-grpc -ocicfs-root /var/lib/ocicfs &
```

The defaults match the config below; see `-help` for the full list.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-address` | `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock` | The snapshotter's own gRPC socket, named again in containerd's `proxy_plugins` entry. |
| `-root` | `/var/lib/containerd-cfs-grpc` | Where the snapshotter keeps its metadata and each snapshot's directories. |
| `-ocicfs-root` | `/var/lib/ocicfs` | The ocicfs store images are served from. |
| `-containerd-address` | `/run/containerd/containerd.sock` | containerd's gRPC socket, whose content store the layers of a staged pull are read back from. |
| `-admit` | `any` | Which images the machine admits: `any` stages every pull or load through containerd's content store and builds it into the ocicfs store; `preloaded` admits only images already in the store (put there with `ocicfs pull`) and declines the rest. |
| `-ingest-workers` | `0` (ocicfs's default, 2) | How many layers staged pulls ingest into the ocicfs store at once, across every pull in progress. Two is the gain on every platform measured; a many-core host with fast storage may take a few percent more from four. |
| `-log-level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`. |
| `-check` | off | Check the runtime prerequisites and the containerd wiring of both the snapshotter and the differ instead of serving: one verdict line per aspect, the effective ingest worker count, and a non-zero exit on any failure. |

The snapshotter root hosts each container's overlay upper and work
directories, so it must live on a regular filesystem (ext4, xfs, …) — overlay
cannot stack on overlay. The ocicfs store's filesystem must support fs-verity
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

The `enable_remote_snapshot_annotations = "true"` export is required. Without
it, transfer-service pulls do not stamp the image identity used by the cfs
admission gate and are declined.

The `cfs-diff` entry is required for staged pulls and for `docker load` in
either mode. Put it first in the diff service's order for Docker, and select
it with `differ` in the cfs `unpack_config` entry for `ctr` and CRI. Other
differs cannot mount the staging placeholder and fail with
`mount: no such device`. `containerd-cfs-grpc -check` checks registration,
but cannot verify the order or the `differ` selection.

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

On Docker 29.9 and later the preloaded mode also needs Docker's `lazy-pull`
feature: without it `docker pull` fetches every layer even when the
snapshotter already holds the image (verified on Docker 29.9.1). Add it to
the `features` object above:

```json
  "features": {
    "containerd-snapshotter": true,
    "lazy-pull": true
  },
```

## 4. Stage, pull, run

This flow is the preloaded mode: it needs the daemon started with
`-admit=preloaded` (with the `-admit=any` default, a pull fetches its
layers instead; see
[Pulling without staging first](#pulling-without-staging-first)).

Stage the image in the ocicfs store first (from a registry or, as in the
devcontainer's tests, from an OCI layout directory):

```sh
sudo ocicfs pull <registry>/<repo>@sha256:<manifest-digest>
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
(`ocicfs pull <registry>/<repo>:<tag>` followed by
`docker pull <registry>/<repo>:<tag>`): ocicfs resolves the tag to the digest
its most recent pull by that tag recorded.

Both pulls complete through cfs with zero layer downloads — only the image
metadata (index, manifest, config, etc.) is fetched: the snapshotter answers
the unpacker's per-layer handshake so the layer chain is recorded as already
present. `ctr snapshots --snapshotter cfs ls` then lists the committed chain
snapshots, and `docker run` serves the image content from the ocicfs store.
Pulling an image the store does not hold fails during the pull at the
snapshotter's admission gate, and running an image whose layer chain is not
in the store fails with an error asking you to pull it by digest out of band.

## Pulling without staging first

By default the store fills from the runtime's own pull instead of from an
out-of-band one — no `ocicfs pull` first:

```sh
sudo ./bin/containerd-cfs-grpc -ocicfs-root /var/lib/ocicfs &

docker pull <registry>/<repo>@sha256:<manifest-digest>
docker run --rm <registry>/<repo>@sha256:<manifest-digest>
```

Nothing else changes: the pull is a plain `docker pull`, with its usual
per-layer progress, and the container gets the same mounts an out-of-band pull
would have given it. What differs is where the content comes from. containerd
fetches the layers it always would and hands each one, as it lands, to the
snapshotter's differ, which ingests it into the ocicfs store's build of the
image — reading the layer back out of containerd's content store, so nothing
is fetched twice and nothing is staged on disk in between. The build scans
layers ahead of the ones containerd has handed over, up to `-ingest-workers`
at a time, so the conversion overlaps the download; when the chain's last
layer is applied the image is finished, verified whole against the manifest
digest, and recorded. From then on a container mounts the image's composefs
like any other. A later pull of the same reference, after the runtime dropped
it, fetches the layers again like any docker pull would — on a staging machine
the runtime's content store keeps the layers of everything it lists, which is
what lets a pull that extends an already-present image read the shared layers
back instead of failing over their absence — and the rebuilt image converges
on the same single composefs image in the store.

The image is the runtime's: the store keeps it by the snapshotter's hold, for
as long as the runtime lists it, and names it by a *runtime reference* — the
reference docker pulled, recorded for that holder. `ocicfs ls` shows the row
like any other (with the holder in `--format json`), and `ocicfs inspect`,
`ocicfs mount` and `ocicfs hold` resolve the name; what the name does not do is
keep the image, and `ocicfs remove` of it declines while the runtime's hold
stands. `docker image rm` is what removes it: containerd forgets the image's
snapshots, the snapshotter releases its hold and collects the store, and the
removal waits for that, so by the time the command returns the image and its
name are gone from `ocicfs ls` and its objects — those no other image shares —
are reclaimed from disk. An image `ocicfs pull` put in the store is an agent's,
and a runtime removal never touches it: its reference keeps it, and only the
hold the runtime took is dropped. An agent that wants a runtime-pulled image
kept adopts it — `ocicfs pull` of the name replaces the runtime's reference
with the agent's — or holds it; once it releases such a hold, it collects
with `ocicfs gc` — reclaiming what an agent let go is the agent's call, not
the snapshotter's. A runtime pull never displaces a reference the agent
recorded for the same name: the image is then held but not named.

A pull writes every file of every layer, and the files a later layer
replaced or removed are objects this image no longer references. A finished
pull does not request a collection for them: those nothing else retains go
with the next one, whoever asks for it — an agent's `ocicfs gc` once the new
image is running, a `docker rmi` that releases a hold, the daemon's start, or
a pull that failed — so on a machine that only ever pulls they stay until one
of those happens, and for an image that writes and later deletes large files
they can be large. A pull that failed or was interrupted is collected in the
background as soon as its build is abandoned. A collection that finds the
store busy — a pull or a mount in flight — is retried by the daemon shortly
after, so a removal during a pull reclaims a little later than the command's
return. A collection that fails for any
other reason is logged and does not fail the docker command; nothing of it
reaches the docker CLI. A hold of the snapshotter's own that no record owns —
what a crash between a finished build and its record leaves behind — is
released when the daemon next starts, which then collects once. An image whose whole layer chain is a
prefix of another image the runtime still lists stays in the store with that
image: containerd keeps its last record as the other's ancestor, and both are
reclaimed when the other goes. The collection is a walk of the whole store,
not a delete proportional to the image, so `docker image rm` costs a store
walk; the walk does not hold up a mount or a pull started meanwhile, and a
collection that meets one still running when it moves on to deleting is
retried the same way as one that found the store busy at its start.
`docker images` reports a staged image's size as its composefs metadata, not
its content.

A `ocicfs gc` while a staged pull is in progress, or shortly after one was
interrupted, reports the store busy: the pull's build keeps the store's shared
lock from its first applied layer until the image is finished, or until
containerd has removed the abandoned pull's snapshots. That is expected;
retry once the pull is over.

Four residuals of routing layers through the differ are worth knowing:

- After a restart of `containerd-cfs-grpc`, containerd's connection to the
  socket takes up to 3 s to come back, and a pull that lands in that window
  fails once. Every Docker-path layer apply tries `cfs-diff` first, so this
  now also holds for an overlayfs pull on the same host; a retry succeeds.
- Two concurrent pulls of the same image in one namespace converge, but the
  losing pull's abort can interrupt the winner's layer apply once; a retry of
  the failed pull converges.
- A `docker rmi` followed at once by a `docker pull` of the same image can
  collide the same way: containerd's collector removes the old pull's layer
  records some moments after the `rmi`, and the snapshotter reads such a
  removal as that pull being unwound, closing the build of the new pull if
  one is under way for the same image. One layer apply of the new pull then
  fails; a retry converges.
- Where containerd's content store lives matters: every layer the store has
  not read before is read back from it, so a slow root filesystem slows the
  ingest. On a device with a faster secondary disk, moving containerd's
  root there is worth more than any worker count.

The two work modes are exclusive, and the flag defaults to `any`:

- **Preloaded (`-admit=preloaded`).** Only images already in the
  store are admitted; every other pull is declined at the snapshotter's
  gate. What runs on the machine is decided by whatever put the images
  there — an update agent pulling digests it verified — and not by whoever
  can reach the runtime.
- **Any (`-admit=any`, default).** Any pull the runtime accepts fills the
  store, and every pull is staged — even of an image the store already holds.
  The content is still verified as it is built, mounted under fs-verity, and
  identical on disk to an out-of-band pull; what `-admit=any` gives up is the
  gate deciding *which* images may arrive, and the preloaded mode's
  zero-download answer to a pull of a store-held image.

Pick one per machine. Switching does not corrupt the store, but images
admitted under `any` keep serving under `preloaded` until removed, so a
device reprovisioned as preloaded should start from a fresh store.

## Loading images offline (`docker load`)

In either mode, sideload the archive into ocicfs with a trusted digest, then
load it into Docker:

```sh
ocicfs pull --from-tar app.tar "registry.example/app@$digest"
docker load -i app.tar
docker run --rm --network=none registry.example/app:1.0
```

`--from-tar` needs ocicfs v0.4.0 or newer. Take `$digest` from
the agent's trusted release metadata; the pull fails unless the archive holds
it. In **`-admit=any`**, `docker load -i app.tar` alone also works: an image
missing from ocicfs is built from the copy `docker load` puts in containerd.

For a multi-platform archive, add `--platform <os>/<arch>` for the host to
each `docker load`, including the check below: for example, `linux/amd64` or
`linux/arm64/v8`. Otherwise Docker also unpacks other platforms the host can
run (linux/386 on amd64) and the snapshotter declines them; amd64+arm64
archives are unaffected.

**Check the output.** `docker load` exits 0 even when it cannot unpack the
image:

```sh
out="$(docker load -i app.tar 2>&1)" || exit 1
if grep -q 'Error unpacking image' <<<"$out"; then echo "$out" >&2; exit 1; fi
```

Out of band a missing image is declined; sideload it and run the image, no
second `docker load` needed. In admit-any mode, a load that only partly
provides an image, such as an incomplete image sharing a chain with a
complete one, succeeds and fails at its first container start instead.

**Images built in admit-any mode.** The last layer's unpack builds the whole
image, so `docker load` takes longer (seconds for a small image). Nothing
pins what is built. `docker rmi` reclaims it from ocicfs. A sideloaded
image stays in ocicfs after `docker rmi` until the agent runs
`ocicfs remove`.

**Load base images before derived images.** A base loaded after a derived
image unpacks nothing, and one loaded into a store holding only the derived
image is answered as part of it; either way it is adopted or built at its
first container start, where its build time and failures surface. In the
preloaded mode, the base must already be in ocicfs; admit-any mode can
rebuild it from containerd's copy.

**Prepare the archive.** Save one tagged image per uncompressed archive with
`docker save -o app.tar <name>:<tag>` on a daemon that uses the containerd
image store, or use `ocicfs copy -o app.tar <name>:<tag>` without a daemon.
A legacy overlay2 daemon rewrites the manifest when it saves, so its
archive matches no published digest. Pin the manifest digest that
`index.json` lists; after `docker save --platform` that is the platform's
manifest, not the multi-platform index:

```sh
tar -xOf app.tar index.json | jq -r '.manifests[0].digest'
```

Docker keeps the imported layer blobs alongside the files in ocicfs, so allow
space for both.

The pin covers the rootfs only; entrypoint, environment and user come from
the archive unchecked — see
[Image-reference stamping and admission](#image-reference-stamping-and-admission).

## Image-reference stamping and admission

The handshake uses `containerd.io/snapshot/cri.image-ref`. Supported runtime
paths stamp it as follows:

| Runtime path | Required configuration |
| --- | --- |
| Docker pull | Docker stamps the reference unconditionally. |
| CRI local pull | Set `disable_snapshot_annotations = false`; the Unix default is `true`. |
| CRI transfer-service pull | Keep the mandatory `enable_remote_snapshot_annotations = "true"` proxy-plugin export shown above. |

The stamped value is the reference exactly as invoked. In the preloaded
mode the snapshotter resolves it against the ocicfs store: a digest reference
must be held, and a tag reference resolves to the digest ocicfs's most recent
pull by that tag recorded — a tag ocicfs holds no alias for is declined. An
admit-any machine stages the pull without consulting the store, but the
reference and manifest digest still anchor the conversion. A missing
annotation is declined in both modes; non-stamping configurations are
unsupported for image-identity
admission. Admit-any mode turns the two declines that mean "the store
does not hold this" into admissions that stage the pull; a reference the
runtime does not stamp at all is still declined, in either mode. If an unsupported client can forge the annotation, chain membership
still limits it to rootfs content belonging to the named held image, but the
snapshotter cannot establish the unheld config's identity.

Pull admission is not a runtime allowlist. Cached images and `docker load`
can bypass image-reference checks: container creation supplies only the
layer chain, which out of band must be in ocicfs. Runtime policy must
control which image configs may use those files. Rootfs digest and
fs-verity checks still apply.

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
image's EROFS metadata mount followed by `::<ocicfs objects>` (a data-only
layer) and `verity=require`; the shared EROFS mount itself appears in the
host's mount table under the snapshotter root
(`.../containerd-cfs-grpc/snapshots/<id>/fs`, fstype `erofs`, on a loop
device). The test image has no shell, so inspect the mounts from the host via
the container's PID as above.
