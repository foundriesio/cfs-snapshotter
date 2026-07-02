# cfs-snapshotter

A [containerd](https://containerd.io/) remote (proxy) snapshotter that serves
container rootfs from content-addressed, composefs-backed storage — using
**composefs + EROFS + fs-verity** to mount a container's filesystem with a single
`mount` syscall instead of unpacking and stacking layers.

> **Status: early development.** The snapshotter skeleton builds and runs as a
> containerd proxy snapshotter, but its methods are currently stubs that return
> `not implemented`; the composefs/CAS backend lands in a later change.

## Build and run

```sh
make build            # writes bin/containerd-cfs-grpc
./bin/containerd-cfs-grpc -version
```

See [docs/running.md](docs/running.md) for running the daemon and registering it
with containerd via a `[proxy_plugins.cfs]` entry.

## Development and testing

The repository ships a privileged devcontainer that runs the snapshotter against
a nested, version-pinned runtime stack — Docker Engine, containerd, runc, CNI
plugins, a local [Zot](https://github.com/project-zot/zot) registry on
`127.0.0.1:5000`, and the locally built `bin/containerd-cfs-grpc`. Versions are
controlled through build arguments in
[`.devcontainer/devcontainer.json`](.devcontainer/devcontainer.json); nothing is
installed from distribution packages.

- **Editors / `devcontainer` CLI:** open the folder in a Dev Containers-aware
  editor ("Reopen in Container") or run `devcontainer up`. The image entrypoint
  starts Zot, `containerd`, `dockerd`, and the cfs proxy snapshotter and waits
  until they are ready.
- **`devshell.sh`:** a host-side helper that builds the same image and runs a
  command inside a privileged container with the nested stack started — e.g.
  `./devshell.sh make test-e2e` to run the fast-path end-to-end test, or
  `./devshell.sh` for an interactive shell. Use `./devshell.sh exec <cmd>` to run
  in an already-running session.

Inside the container, `make run-cfs` rebuilds and restarts the snapshotter after
editing code, and `make test-e2e` checks that a Docker run is served from a
composefs mount with no layer fetch. See
[docs/devcontainer.md](docs/devcontainer.md) for the full workflow.

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md) first. All commits must be signed off under
the [Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`), and pull requests must be opened and reviewed by a human.
Automated coding agents must additionally follow [AGENTS.md](AGENTS.md).

## License

cfs-snapshotter is licensed under the [Apache License 2.0](LICENSE).

Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
