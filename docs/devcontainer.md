<!-- SPDX-License-Identifier: Apache-2.0 -->

# Devcontainer runtime testing

The privileged devcontainer runs a nested Docker/containerd stack and a
local Zot registry at `127.0.0.1:5000`. Both runtime clients are configured
for cfs. This is a development environment; the snapshotter itself still
only provides storage primitives.

| You want | Start from the host |
|---|---|
| Interactive development, including e2e | `./devshell.sh` |
| One command in a fresh container | `./devshell.sh make test-e2e` |
| Another shell or command in the running session | `./devshell.sh exec` or `./devshell.sh exec <command>` |
| An editor-managed session | Reopen in Container; the ocicfs CLI must be supplied separately for e2e. |

## 1. Prepare the host

Use a Linux Docker host that allows privileged containers, loop devices
and nested mounts. It needs the
[runtime kernel features](../INSTALL.md#runtime-requirements), including
fs-verity for the verification tests, and network access to build the
image and fetch dependencies. `devshell.sh` also needs Go on the host to
build the pinned ocicfs CLI.

The image is based on Debian Bookworm via
`mcr.microsoft.com/devcontainers/go:<GO_VERSION>-bookworm`. Docker,
containerd, runc, CNI plugins and Zot come from upstream release archives.
It builds erofs-utils and composefs from source. ocicfs uses `mkcomposefs`
when building images; mounting uses its Go implementation.

## 2. Open a shell and build

```sh
./devshell.sh
```

Startup ends with these lines before handing over to the requested shell
or command:

```text
devcontainer daemons are ready
logs: /var/log/cfs-devcontainer
```

Inside the container:

```sh
make check
make build
make run-cfs
```

`make run-cfs` rebuilds the daemon and restarts it if its binary or settings
changed. It leaves an unchanged process running unless
`CFS_FORCE_RESTART=1` is set. It restarts containerd only if the cfs
snapshotter plugin is not registered.

## 3. Check a container

The stack starts in admit-any mode. Seed a runnable image into the local
registry, pull it and run it:

```sh
image=127.0.0.1:5000/cfs/demo:latest
SEED_RUNNABLE=1 SEED_NONCE=demo \
    .devcontainer/scripts/seed-registry.sh "$image"
docker pull "$image"
docker run --rm "$image"
```

Expected container output:

```text
cfs hello demo
```

Inspect Docker's snapshots and daemon log:

```sh
ctr --namespace moby snapshots --snapshotter cfs ls
cfs-log
```

The shell helpers `cfs-log` and `cfs-log-follow` pretty-print
`/var/log/cfs-devcontainer/containerd-cfs-grpc.log` using `jq`.
[Keep the container running](running.md#inspecting-a-running-container-devcontainer)
to inspect its mounts.

## Try preloaded mode

Use a new image so existing runtime snapshots do not bypass admission:

```sh
CFS_ADMIT=preloaded make run-cfs
layout=$(mktemp -d)
SEED_RUNNABLE=1 SEED_NONCE=preloaded SEED_KEEP_LAYOUT="$layout" \
    .devcontainer/scripts/seed-registry.sh 127.0.0.1:5000/cfs/preloaded:latest
digest=$(jq -r '.manifests[0].digest' "$layout/index.json")
image="127.0.0.1:5000/cfs/preloaded@$digest"
ocicfs pull --from-dir "$layout" "$image"
docker pull "$image"
docker run --rm "$image"
```

Expected output from the container:

```text
cfs hello preloaded
```

The runtime needs only image metadata for this pull. An image absent from
the store is declined. Return to admit-any mode with `make run-cfs`, with
`CFS_ADMIT` unset. See [admission limits](running.md#image-reference-stamping-and-admission)
before treating mode changes as a deployment policy.

## End-To-End Test

Run from the host:

```sh
./devshell.sh make test-e2e
```

Or run `make test-e2e` inside a devshell session. It depends on
`make run-cfs`, then runs `.devcontainer/scripts/e2e-fastpath.sh`. It
changes runtime state, seeds images and switches the daemon between modes.
Use the dedicated development stack.

The suite checks these flows:

| Check group | Store and checks |
|---|---|
| Preloaded | Reject absent images and config substitutions on fresh pulls; check layer absence in containerd, container output, holds, shared EROFS mounts, recovery after unmount/restart, loop teardown and tamper rejection. |
| Delta preload | Pull two zstd:chunked fixture releases, check range-transfer counters, compare the resulting image, export changed/added/removed files through cfs, and collect the old base. |
| Admit-any | Use `/var/lib/ocicfs-cold` and `/var/lib/containerd-cfs-grpc-cold`; check incremental ingest, mount options, refetch, shared objects, runtime-reference retention, orphan holds, interrupted pulls and cleanup. Compare builds at one and two workers and with a plain ocicfs pull. |
| Offline loads | In both modes, load with the registry stopped; check missing/preloaded images, base-after-derived ordering and recovery. Also check sideload followed by a digest pull before disconnecting. |
| Without fs-verity | Use `/var/lib/ocicfs-tmpfs` on tmpfs and `/var/lib/containerd-cfs-grpc-tmpfs` on ext4. Check the startup warning, pull/run output and an overlay without `verity=require`. |

The cold tests also check that files superseded by later layers remain
after pull and are reclaimed by an explicit `ocicfs gc`. Agent holds keep
images after runtime removal until released and collected. A retained
derived image keeps its base chain. The test's immediate-reclamation
assertions apply to its controlled, idle store; operational limits are in
[retention and collection](running.md#image-retention-and-garbage-collection).

The script prints `ok` or `skip` for individual checks and exits non-zero
on failure. A successful run ends with `E2E OK: <passed> passed, <skipped>
skipped`, with counts for that host. It reports per-layer timings as
`apply: <image> layer <i> in <T>s`, or
`finish: <image> layer <i> in <T>s` for the final layer. These
come from the differ's elapsed-time logs, not separate benchmarks.
Layer absence and registry counters are the suite's evidence for skipped
fetches; they are not packet captures.

## Host Requirements

The prerequisites are listed in [step 1](#1-prepare-the-host). For targets
that do not need a nested runtime, use the host directly:

| Target | Where it runs |
|---|---|
| `make`, `build`, `fmt`, `clean` | Host or devcontainer. |
| `install` | Either; requires permission to write `PREFIX`/`DESTDIR`. |
| `check`, `test`, `vet`, `lint` | Either with Go and, for lint/check, golangci-lint. Some integration tests skip when tools or privileges are absent. |
| `run-cfs`, `test-e2e` | Devcontainer; e2e also needs the ocicfs CLI. |
| `root-test` | Privileged environment; see limitations below. |
| `deb`, `deb-lint`, `deb-test` | Host with Go and Docker; packaging uses its own container. |

## Version Knobs

`devshell.sh` builds directly from
[.devcontainer/Dockerfile](../.devcontainer/Dockerfile). Editor sessions
apply the build arguments in
[devcontainer.json](../.devcontainer/devcontainer.json). Editing those
JSON arguments does not change a devshell build.

| Component | Dockerfile default | Editor override |
|---|---|---|
| Go image series | `1.26` | `1.26` |
| Docker | `29.3.0` | same |
| containerd | `2.2.2` | same |
| runc | `1.3.3` | same |
| CNI plugins | `1.8.0` | same |
| Zot | `2.1.17` | same |
| erofs-utils | `1.9.2` | same |
| golangci-lint | `2.12.2` | `2.6.2` |

The Go version required by the module is in `go.mod`. composefs is pinned
by `COMPOSEFS_VERSION` and `COMPOSEFS_COMMIT` in the Dockerfile. Missing
release archives fail the image build. Update the appropriate build
arguments and rebuild when testing another runtime version.

The ocicfs CLI is not baked into the image. `devshell.sh` tries to build
exactly the module version pinned in `go.mod` on the host, then mounts it
at `/opt/cfs-dev/bin/ocicfs`. A failed build warns but still opens the
shell; e2e then cannot run. An editor-opened container does not receive
that binary automatically.

Devshell keeps Go caches and the CLI under
`${XDG_CACHE_HOME:-$HOME/.cache}/cfs-snapshotter`. Override the directory
with `CFS_DEV_CACHE`. They survive sessions and avoid a small `/tmp`.
Editor sessions use their configured in-container caches instead. To
prefetch into devshell's default module cache as CI does:

```sh
GOMODCACHE="$HOME/.cache/cfs-snapshotter/go-mod" go mod download
```

Adjust the path when overriding the cache location.

## Startup

The entrypoint starts Zot, builds cfs and prepares a loop-backed ext4
filesystem with fs-verity. It binds subdirectories onto the ocicfs and
snapshotter roots. This gives verification tests fs-verity and gives
overlay upper/work directories a filesystem other than the container's
own overlayfs. fs-verity is optional for normal cfs operation.

`CFS_SHARED_ROOT_SIZE_MB` selects the size (default 1024 MiB) when the
filesystem is created. Devshell and the editor configuration pass it
from the host. An already mounted shared root keeps its current size.

The entrypoint starts cfs before containerd, then starts dockerd with an
explicit connection to that containerd. It waits for the runtime clients
to answer before executing your command. Docker and containerd allow the
local HTTP registry. Logs and pid files live under
`/var/log/cfs-devcontainer/`.

The daemon starts with trace logging, admit-any mode and the default
worker count. Change settings for a subsequent run with:

```sh
CFS_LOG_LEVEL=debug CFS_INGEST_WORKERS=2 make run-cfs
```

`CFS_ROOT` and `OCICFS_ROOT` select alternate roots for the run script.
`CFS_ADMIT=preloaded` selects preloaded mode. Unset overrides return to
the script defaults on the next `make run-cfs`.

## Conformance Suite

`make root-test` invokes containerd's reusable `SnapshotterSuite` with
`-test.root`. Plain `make test` skips it. It needs a privileged environment
capable of mounting and preserving file ownership:

```sh
./devshell.sh make root-test
```

The suite prepares parentless scratch snapshots. cfs requires an image
ancestor and rejects that shape with an error beginning:

```text
snapshot has no image in its ancestry; scratch snapshots are not supported
```

This prevents those cases from reaching their layer semantics, whiteout,
rename and deep-stack checks. The suite is not currently a passing
conformance gate. The e2e exercises supported image-backed flows; it is
not a substitute for resolving the conformance mismatch. Do not modify
the upstream suite to hide the failure.

## Debugging Niceties

Use the [log helpers](#3-check-a-container) and the
[mount inspection example](running.md#inspecting-a-running-container-devcontainer).
The seeded `/hello sleep` process handles termination, so `docker stop`
does not need to wait for a forced kill.

## Comparing Against The erofs Snapshotter

The entrypoint enables containerd's built-in erofs snapshotter when
`/proc/filesystems` lists erofs. Without it, startup prints:

```text
host kernel lacks EROFS; erofs snapshotter comparison disabled
```

That avoids registering an unusable comparison plugin; cfs itself still
needs EROFS to serve containers. To compare on a capable host:

```sh
ctr images pull --snapshotter erofs docker.io/library/busybox:latest
ctr run --rm --snapshotter erofs \
    docker.io/library/busybox:latest erofs-demo echo hello
ctr snapshots --snapshotter erofs ls
```

The container prints `hello`. This local snapshotter downloads layers and
converts each to EROFS during unpack; it does not use cfs's preloaded
handshake. The image builds erofs-utils with the tar support it needs.

## Shutdown And Cleanup

Stopping the devcontainer stops its nested daemons. Devshell removes its
container on exit and hands cache ownership back to the host user.

To stop the daemons and delete their state inside a development session:

```sh
.devcontainer/scripts/clean-runtime-state.sh --force
```

The script removes `/var/lib/docker`, `/var/lib/containerd`,
`/var/lib/containerd-cfs-grpc`, `/var/lib/cfs-dev-registry`, the related
runtime socket directories and `/var/log/cfs-devcontainer`. It does not
remove the ocicfs store or unmount the shared filesystem. Start a fresh
devshell session for a fresh nested stack.
