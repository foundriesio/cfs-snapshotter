<!-- SPDX-License-Identifier: Apache-2.0 -->

# Devcontainer Runtime Testing

This repository includes a privileged devcontainer for testing the cfs proxy
snapshotter against a nested, version-pinned runtime stack:

- Docker Engine from Docker's static binary archive.
- Upstream containerd from containerd release assets.
- Upstream runc.
- CNI plugins.
- Zot, used as a local test registry on `127.0.0.1:5000`.
- erofs-utils built from source (Debian's package predates the tar mode that
  containerd's built-in erofs snapshotter needs).
- The locally built `bin/containerd-cfs-grpc`.

The container is intentionally for development and conformance-style wiring
checks only. It does not add Docker or container lifecycle behavior to
cfs-snapshotter itself.

## Host Requirements

- Linux Docker host.
- Privileged containers enabled.
- Kernel support for nested mounts and the snapshotter operations being tested.
- Network access during devcontainer build to fetch the requested binary
  release archives.

The devcontainer uses Debian Bookworm through
`mcr.microsoft.com/devcontainers/go:<GO_VERSION>-bookworm`. It does not install
Docker or containerd from Debian packages, so runtime versions are controlled
only through build arguments.

## Version Knobs

Override versions in `.devcontainer/devcontainer.json` by changing the build
arguments:

```jsonc
"build": {
  "dockerfile": "Dockerfile",
  "args": {
    "GO_VERSION": "1.25",
    "DOCKER_VERSION": "29.3.0",
    "CONTAINERD_VERSION": "2.2.2",
    "RUNC_VERSION": "1.3.3",
    "CNI_PLUGINS_VERSION": "1.8.0",
    "ZOT_VERSION": "2.1.17",
    "EROFS_UTILS_VERSION": "1.9.2",
    "GOLANGCI_LINT_VERSION": "2.6.2"
  }
}
```

The Dockerfile downloads exact release artifacts and fails during build if a
requested archive is missing or cannot be installed.

Reference release locations:

- Docker static binaries: <https://docs.docker.com/engine/install/binaries/>
- containerd release assets: <https://github.com/containerd/containerd/releases>
- Zot release assets: <https://github.com/project-zot/zot/releases>
- erofs-utils release tags: <https://github.com/erofs/erofs-utils/tags>

## Startup

Opening the devcontainer runs the image entrypoint. The entrypoint:

- Starts Zot, `containerd`, and `dockerd`.
- Starts `bin/containerd-cfs-grpc` before `containerd`, so containerd can
  register the `cfs` proxy snapshotter during startup.
- Waits for `ctr version`, `docker version`, and `docker info`.
- Runs the requested command, or the default `sleep infinity` command for an
  interactive devcontainer session.

The Dockerfile bakes in `/etc/containerd/config.toml` with the `cfs` proxy
snapshotter and unpack entries for `overlayfs` and `cfs`. It also bakes in
`/etc/docker/daemon.json` with Docker's containerd image store enabled and `cfs`
as the selected storage driver. Both Docker and containerd are configured to use
the local insecure registry at `127.0.0.1:5000`.

Logs and pid files are written under `/var/log/cfs-devcontainer/`.

Rebuild and restart the cfs proxy snapshotter after editing snapshotter code:

```sh
make run-cfs
```

`make run-cfs` builds `bin/containerd-cfs-grpc`, restarts it in the background,
and ensures containerd sees the `cfs` proxy snapshotter. It does not restart
containerd after every cfs restart; it restarts containerd only if the `cfs`
proxy plugin is not registered.

The cfs snapshotter runs with trace logging by default in the devcontainer. To
override it:

```sh
CFS_LOG_LEVEL=debug make run-cfs
```

## Smoke Test

From the host, run a command inside the devcontainer with the nested daemon
stack started:

```sh
./devshell.sh make test-e2e
```

From an already-open devcontainer, run the smoke test:

```sh
make test-e2e
```

`make test-e2e` depends on `make run-cfs`, so it rebuilds and restarts the cfs
proxy before checking Docker and containerd routing.

Run:

```sh
.devcontainer/scripts/e2e-fastpath.sh
```

The smoke test prints the actual runtime versions, runs `make fmt`, `make vet`,
`make test`, and `make build`, checks that containerd lists the `cfs` proxy
snapshotter, seeds Zot with a tiny local OCI image, then verifies the stub end
to end: both `ctr` and Docker pulls complete through cfs with zero layer
fetches (the unpack handshakes are answered from the stub; only the image
metadata is downloaded), and `docker run` executes the stand-in rootfs.

## Debugging Niceties

Two shell functions and an environment variable make interactive debugging
easier from any login shell inside the devcontainer (`./devshell.sh`, a VS Code
terminal, etc.):

- `cfs-log` -- pretty-print the whole `containerd-cfs-grpc` daemon log.
- `cfs-log-follow` -- tail and pretty-print the log as new entries arrive.

Both read `/var/log/cfs-devcontainer/containerd-cfs-grpc.log` (see Startup
above) through `jq`.

A plain `docker run` against the devcontainer's stand-in image content exits
immediately once it prints its greeting, leaving nothing to inspect. Setting
`CFS_STUB_HOLD` in the container's environment makes it print and then block
instead, so the container and its overlay mount stay up for inspection from the
host. See
[docs/running.md](running.md#inspecting-a-running-container-devcontainer-stub)
for the full example.

## Comparing Against The erofs Snapshotter

containerd's built-in erofs snapshotter is wired up as a reference point: it
converts each layer to an EROFS blob at unpack time and mounts the result,
which makes it a useful side-by-side comparison for cfs's snapshot and mount
shapes. Ask for it explicitly (the devcontainer default is cfs):

```sh
ctr image pull --snapshotter=erofs docker.io/library/busybox:latest
ctr run --rm --snapshotter=erofs docker.io/library/busybox:latest erofs-demo echo hello
ctr snapshots --snapshotter erofs ls
```

Unlike cfs, these pulls download the layers: the erofs snapshotter is a local
snapshotter, not a remote one, so no unpack handshake is answered and the
transfer service fetches and converts every layer.

This needs EROFS support in the host kernel. The entrypoint enables the erofs
snapshotter only when `/proc/filesystems` lists erofs; on hosts without it
(CI runners included) it prints a notice and leaves erofs out, and cfs is
unaffected either way.

## Shutdown And Cleanup

Stopping the devcontainer stops the nested daemons with the container.

Delete nested daemon state and logs:

```sh
.devcontainer/scripts/clean-runtime-state.sh --force
```

The cleanup script requires `--force` because it removes `/var/lib/docker`,
`/var/lib/containerd`, `/var/lib/containerd-cfs-grpc`,
`/var/lib/cfs-dev-registry`, related `/run` sockets, and
`/var/log/cfs-devcontainer` inside the devcontainer.
