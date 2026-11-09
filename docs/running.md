<!-- SPDX-License-Identifier: Apache-2.0 -->

# Running cfs-snapshotter

The `containerd-cfs-grpc` daemon serves the Snapshots and Diff APIs on
one Unix socket. containerd calls them to prepare filesystems and ingest
image layers. Docker uses them through containerd.

First complete [installation and runtime configuration](../INSTALL.md).
For a development stack with Docker, containerd and a local registry
already configured, use the [devcontainer guide](devcontainer.md).

| How you deliver images | Follow |
|---|---|
| Let the runtime fill the store (default) | [Pulling without staging first](#pulling-without-staging-first) |
| Preload images with an update agent | [Preload, pull and run](#preload-pull-and-run) |
| Deliver an archive offline | [Loading images offline](#loading-images-offline-docker-load) |

## Pulling without staging first

### 1. Start in admit-any mode

The installed service defaults to `-admit=any`. If you changed it, set
`CFS_OPTS="-admit=any"` in `/etc/default/cfs-snapshotter` and restart:

```sh
sudo systemctl restart cfs-snapshotter
sudo containerd-cfs-grpc -check -admit=any
```

The check should end with `check passed`. For a source build without
systemd, keep this running in another terminal:

```sh
sudo ./bin/containerd-cfs-grpc -admit=any
```

### 2. Pull and run

```sh
image=docker.io/library/busybox:latest
docker pull "$image"
docker run --rm "$image" echo 'it works'
```

Expected container output:

```text
it works
```

For containerd without Docker:

```sh
image=docker.io/library/busybox:latest
sudo ctr images pull --snapshotter cfs "$image"
sudo ctr run --rm --snapshotter cfs "$image" cfs-demo echo 'it works'
```

This prints the same greeting. Tags are convenient for a trial; use
`name@sha256:<digest>` from trusted release metadata in production.

### 3. Inspect the records

Docker uses the `moby` namespace. A `ctr` command without a namespace uses
`default`, so inspect the namespace used for the pull:

```sh
# Docker's records
sudo ctr --namespace moby snapshots --snapshotter cfs ls
# The ctr example above
sudo ctr snapshots --snapshotter cfs ls
```

Expect committed image-chain snapshots. A running container adds its
own snapshots. `docker images` size accounting uses snapshot usage;
image-chain usage counts composefs metadata, not all shared file data.

### What a runtime pull does

containerd downloads layer blobs into its content store. The cfs differ
reads them from there into an ocicfs build as each layer is applied.
There is no second registry fetch or separate cfs layer-staging directory.
The store scans ahead up to `-ingest-workers` layers across all builds.
The final layer finishes and records the image. ocicfs verifies metadata
and layer digests during construction.

New unpack handshakes in admit-any mode use this path even if ocicfs
already has the image. containerd may reuse snapshots or blobs it still
has. After runtime removal and collection, another pull can download the
layers again. Keeping layer blobs in containerd also lets a later image
reuse a base whose chain is already recorded. Allow disk space for those
blobs as well as the ocicfs store.

## Preload, pull and run

### 1. Select preloaded mode

Set this line in `/etc/default/cfs-snapshotter`:

```sh
CFS_OPTS="-admit=preloaded"
```

Then restart and check:

```sh
sudo systemctl restart cfs-snapshotter
sudo containerd-cfs-grpc -check -admit=preloaded
```

For a source build without systemd, start
`sudo ./bin/containerd-cfs-grpc -admit=preloaded` in another terminal.
On Docker 29.9 or newer, also enable lazy pulling as described in
[Docker configuration](../INSTALL.md#2-connect-docker-if-used).

### 2. Preload an image

For a trial with BusyBox:

```sh
image=docker.io/library/busybox:latest
sudo ocicfs pull "$image"
```

Use the same store root as the daemon. For production, set `image` to a
trusted digest-pinned reference. Preloading also accepts OCI layouts and
archives. `ocicfs pull --delta` can reduce downloads for zstd:chunked
layers; see the [ocicfs pull guide](https://github.com/foundriesio/ocicfs/blob/main/docs/pull.md).

### 3. Pull and check a container

```sh
docker pull "$image"
docker run --rm "$image" echo 'it works'
```

Expected container output:

```text
it works
```

With containerd alone, use the `ctr` pull and run commands from the
admit-any walkthrough. An admitted fresh pull can skip all layer
fetches; the runtime still fetches the index, manifest and config. This
requires a client configured to honor the remote-snapshotter handshake.

A tag works only if ocicfs recorded that alias and the requested chain
belongs to the stored image. If the registry tag moves, preload the new
image before pulling it through the runtime. A pull of an image absent
from the store is declined. If a container's complete image chain is
unavailable, its start fails and asks you to run `ocicfs pull`.

## Loading images offline (`docker load`)

Both modes need the proxy differ configured. A missing or mis-ordered
differ makes the layer apply fail with `mount: no such device`; check the
[containerd configuration](../INSTALL.md#1-register-the-snapshotter-and-differ).
Choose the import path:

| Mode | Required preparation |
|---|---|
| `any` | `docker load` can build a missing image from the imported blobs. |
| `preloaded` | Import the image into ocicfs first with a trusted digest. |

### 1. Prepare the archive

On a connected machine, save one tagged image per uncompressed archive.
Use a Docker daemon with the containerd image store and all layer blobs
present:

```sh
docker save -o app.tar registry.example/app:1.0
```

Or use ocicfs without a Docker daemon:

```sh
ocicfs copy -o app.tar registry.example/app:1.0
```

A legacy overlay2 save rewrites the manifest, so its archive does not
preserve the published digest. A preloaded runtime may lack layer blobs
and cannot reliably save a complete archive; use the connected source.

For inspecting what the archive names:

```sh
tar -xOf app.tar index.json | jq -r '.manifests[0].digest'
```

After `docker save --platform`, this names the platform manifest rather
than the original multi-platform index. Inspection is not authentication:
the digest used on the target must come from trusted release metadata.

### 2. Import into ocicfs when preloaded

Set `digest` to that trusted digest, then:

```sh
sudo ocicfs pull --from-tar app.tar "registry.example/app@$digest"
```

The archive must contain that digest. `--from-tar` requires ocicfs v0.4.0
or newer; use the CLI version recommended in
[installation requirements](../INSTALL.md#runtime-requirements).
This step is optional in admit-any mode.

### 3. Load and check the result

Run this in Bash. `docker load` can exit successfully while reporting an
unpack failure, so inspect its output too:

```bash
out="$(docker load -i app.tar 2>&1)" || { printf '%s\n' "$out" >&2; exit 1; }
printf '%s\n' "$out"
if grep -q 'Error unpacking image' <<<"$out"; then exit 1; fi
```

For a multi-platform archive, add `--platform linux/amd64` or
`--platform linux/arm64/v8` for the host to the `docker load` command.
Otherwise Docker can attempt another runnable platform, such as 386 on
amd64, which cfs declines. An amd64+arm64 archive does not have that
particular ambiguity.

For an archive carrying the tag used above, expect this line and no
unpack error. An `ocicfs copy` archive of a multi-platform image adds a
second `Loaded image:` line naming the manifest digest:

```text
Loaded image: registry.example/app:1.0
```

Then run its normal command without network access:

```sh
docker run --rm --network=none registry.example/app:1.0
```

Check the application's expected output. A successful load alone is not
proof that the container can start.

### Offline loading notes

- A missing preloaded image is declined during unpack. After sideloading
  it, run the imported image; a second `docker load` is not needed.
- In admit-any mode, a partial archive sharing a chain with a complete
  image can load successfully but fail at the first container start.
- Load base images before derived images. A base loaded later may have
  no new layers to unpack. Its first container start must adopt a stored
  image ending at that chain, or build one from complete containerd
  content in admit-any mode. That defers build time and errors to start.
  In preloaded mode, the base must already be in ocicfs.
- Building the image can add time to the last layer's unpack. Runtime
  holds keep it alive until its snapshots are removed; an ordinary
  `ocicfs pull` reference survives runtime removal. See retention below.
- Docker retains imported layer blobs alongside the ocicfs files.
- An `ocicfs copy` archive of a multi-platform image loads under two
  names, the tag and `<repo>@<manifest digest>`. Run `docker rmi` on both
  (every `Loaded image:` line) before `ocicfs remove` or `gc` can release
  the image.
- The preload pin checks rootfs content. The runtime still takes
  entrypoint, environment and user from the imported config. See
  [admission and trust](#image-reference-stamping-and-admission).

As an alternative, preload the archive, pull its digest through the
runtime while the registry is reachable, then disconnect. That runtime
pull needs metadata only. A digest-only preload does not record a tag
alias for a later runtime pull by tag.

## Image retention and garbage collection

The snapshotter takes a **hold**, an ocicfs retention record, when it
adopts or builds an image. A runtime-built image gets a runtime reference:
`ocicfs ls`, `inspect`, `mount` and `hold` can resolve the name, but the
name itself does not retain content. `ocicfs ls --format json` shows its
holder. `ocicfs remove` declines while the hold exists.

When containerd removes the owning snapshots, cfs releases their holds
and collects unreferenced store content. `docker image rm` initiates this
process; reclamation depends on containerd cleanup and store activity,
so command return is not a universal reclamation deadline. A chain that
is an ancestor of another image remains until that image no longer needs
it. Shared objects remain while any retained image uses them.

An ordinary `ocicfs pull` reference keeps an image independently of the
runtime. A runtime pull never replaces that reference. To retain a
runtime-built image independently, pull its name with ocicfs or take an
ocicfs hold. After releasing your own hold, run `ocicfs gc`; the
snapshotter does not collect just because another owner released one.

A finished pull does not collect files superseded or deleted by later
layers. Those objects can occupy substantial space until the next
collection: an explicit `ocicfs gc`, runtime hold release, daemon startup
or abandoned build. Failed or interrupted builds are collected once
abandoned. On startup, the daemon also releases its own holds that no
snapshot record owns, then requests collection.

Collection walks the whole store. It can overlap a new pull during its
inventory, but defers deletion when the store is busy. cfs retries busy
collections; other errors are logged and returned from its cleanup RPC.
Do not rely on Docker displaying those errors. A manual `ocicfs gc` can
report the store busy from the first applied layer until a build finishes
or its abandoned snapshots are removed. Retry after that work ends.

## Image-reference stamping and admission

Fresh pull admission uses `containerd.io/snapshot/cri.image-ref`:

| Runtime path | Configuration |
|---|---|
| Docker pull | Docker stamps the invoked reference. |
| CRI local pull | Set `disable_snapshot_annotations = false` (the Unix default is `true`). |
| Transfer-service pull (`ctr` or CRI) | Set `enable_remote_snapshot_annotations = "true"` in the cfs proxy exports. |

In preloaded mode, cfs resolves the stamped reference locally and checks
that the requested chain belongs to that image. A digest can resolve a
stored image directly; an existing hold is not a prerequisite. A tag
needs the alias recorded by ocicfs. Admission of the final chain takes
the snapshotter's own hold.

In admit-any mode, a fresh labeled handshake selects staging before
consulting the store. The manifest digest and reference anchor the build.
A missing image-reference annotation is declined in either mode.

This is pull admission, not a runtime allowlist. Cached snapshots and
archive loads can bypass reference checks. Container creation supplies
layer chains; runtime policy must control which configs may use those
files. A client forging the reference cannot use that admission path for
an unrelated rootfs chain, but cfs does not authenticate its config.

ocicfs verifies the stored metadata chain and expected EROFS digest when
mounting. For sealed images, the expected digest comes from a manifest
annotation; for unsealed images it comes from the local image record.
With fs-verity, the overlay enforces backing object digests on reads.
Without it, backing file reads are not verified. Container writes are
outside that protection, and digest verification does not establish
publisher identity.

Changing admit modes does not clear runtime snapshots or the store.
Images admitted under `any` can still serve under `preloaded`. To
reprovision a device with a new admission policy, start with fresh runtime
snapshot state and a deliberately populated store.

## Daemon flags

| Flag | Default | Meaning |
|---|---|---|
| `-address` | `/run/containerd-cfs-grpc/containerd-cfs-grpc.sock` | Shared snapshotter and differ socket. |
| `-root` | `/var/lib/containerd-cfs-grpc` | Snapshot metadata and overlay directories. |
| `-ocicfs-root` | `/var/lib/ocicfs` | Shared image store. |
| `-containerd-address` | `/run/containerd/containerd.sock` | containerd socket for probing and reading runtime content. |
| `-admit` | `any` | `any` ingests runtime content; `preloaded` requires stored images. |
| `-ingest-workers` | `0` (selects 2) | Concurrent layer scans across builds in this daemon; negative values are invalid. |
| `-log-level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`. |
| `-version` | false | Print version and revision, then exit. |
| `-check` | false | Report tool/store prerequisites and containerd registration, then exit; see [check limits](../INSTALL.md#check-the-installation). |

Run `containerd-cfs-grpc -help` for help. The
[storage and fs-verity requirements](../INSTALL.md#runtime-requirements)
apply in both admit modes.

## Operational notes

- After restarting the snapshotter, pulls can fail while containerd
  reconnects. Retry once the socket is serving. Docker tries `cfs-diff`
  first even for another snapshotter, so its pulls can be affected too.
- Concurrent pulls of the same image in one namespace share build state.
  Aborting one can interrupt the other's layer apply. An immediate
  remove-and-repull can also race cleanup of old records. Retry the
  failed pull after cleanup settles.
- Ingest reads layers back from containerd's content store. Its storage
  speed matters as well as the ocicfs store's speed; tune placement
  before increasing worker counts. No worker setting avoids those reads.
- Parentless scratch snapshots are unsupported. The generic conformance
  suite therefore cannot yet validate normal snapshot semantics; see
  [conformance limitations](devcontainer.md#conformance-suite).

## Inspecting a running container (devcontainer)

The [seeded test image](devcontainer.md#3-check-a-container) has a static
`/hello` binary and no shell. Normally it prints a greeting and exits.
Use its `sleep` argument to retain the container for inspection:

```sh
docker run --rm -d --name cfs-hold "$image" /hello sleep
pid="$(docker inspect -f '{{.State.Pid}}' cfs-hold)"
cat "/proc/${pid}/mountinfo"
docker stop cfs-hold
```

The root is an overlay whose lower directories end with the EROFS
metadata and `::<ocicfs objects>` (a data-only lower layer). It has
`verity=require` only when the store runs with fs-verity. The shared
EROFS mount appears in the host mount table at
`<snapshotter-root>/snapshots/<id>/fs`, backed by a loop device.
