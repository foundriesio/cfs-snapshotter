<!-- SPDX-License-Identifier: Apache-2.0 -->

# Devcontainer Runtime Testing

This repository includes a privileged devcontainer for testing the cfs proxy
snapshotter against a nested, version-pinned runtime stack:

- Docker Engine from Docker's static binary archive.
- Upstream containerd from containerd release assets.
- Upstream runc.
- CNI plugins.
- Zot, used as a local test registry on `127.0.0.1:5000`.
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

## Startup

Opening the devcontainer runs the image entrypoint. The entrypoint:

- Starts Zot, `containerd`, and `dockerd`.
- Builds and starts `bin/containerd-cfs-grpc` before `containerd`, so
  containerd can register the `cfs` proxy snapshotter during startup.
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
./devshell.sh make test-smoke
```

From an already-open devcontainer, run the smoke test:

```sh
make test-smoke
```

`make test-smoke` depends on `make run-cfs`, so it rebuilds and restarts the cfs
proxy before checking Docker and containerd routing.

Run:

```sh
.devcontainer/scripts/smoke-test-cfs-routing.sh
```

The smoke test prints the actual runtime versions, runs `make fmt`, `make vet`,
`make test`, and `make build`, checks that containerd lists the `cfs` proxy
snapshotter, seeds Zot with a tiny local OCI image, then verifies that both
`ctr` and Docker route pulls to cfs.

Current expected behavior: both pulls reach cfs and fail at `Prepare` with
`not implemented`. That proves the routing is wired — both clients reach the cfs
snapshotter through the proxy.

Future expected behavior: after the composefs backend exists, the pulls succeed
and `docker run --rm 127.0.0.1:5000/cfs/smoke:latest true` runs through the cfs
snapshotter.

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
