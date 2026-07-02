# cfs-snapshotter

A [containerd](https://containerd.io/) remote (proxy) snapshotter that serves
container rootfs from content-addressed, composefs-backed storage — using
**composefs + EROFS + fs-verity** to mount a container's filesystem with a single
`mount` syscall instead of unpacking and stacking layers.

> **Status: early development.** The snapshotter serves images staged out of
> band in a local [knarr](https://github.com/foundriesio/knarr) store: pulls
> routed to cfs download no layers (`ctr` and `docker` still fetch the image
> metadata: index, manifest, config, etc.), and `docker run` executes the
> image's real content from a shared composefs-backed mount with fs-verity
> enforced.

## Build and run

```sh
make build            # writes bin/containerd-cfs-grpc
./bin/containerd-cfs-grpc -version
```

See [docs/running.md](docs/running.md) for running the daemon and registering it
with containerd via a `[proxy_plugins.cfs]` entry.

## Development and testing

Develop and test inside the repository's privileged devcontainer: it runs a
nested runtime stack — `dockerd`, `containerd`, `runc`, a local
[Zot](https://github.com/project-zot/zot) registry on `127.0.0.1:5000` — with
the locally built snapshotter already wired in. Get in either way:

- open the folder in a Dev Containers-aware editor ("Reopen in Container"), or
- run `./devshell.sh` from the host (interactive shell; `./devshell.sh <cmd>`
  runs one command, `./devshell.sh exec <cmd>` reuses the running session).

Inside the container:

```sh
make run-cfs      # rebuild and restart the snapshotter after editing code
make test         # unit tests
make test-smoke   # end to end: ctr/docker pull and docker run through cfs
```

To test manually, drive the snapshotter with plain `docker` and `ctr`
commands — both are preconfigured to use cfs, and any image from a regular
remote registry works (busybox here) — then watch the snapshotter's logs.
Layers are never downloaded; only the image metadata (index, manifest,
config, etc.) is fetched:

```sh
docker pull busybox
docker run --rm busybox
ctr image pull docker.io/library/busybox:latest
ctr run --rm docker.io/library/busybox:latest demo
ctr snapshots ls                  # committed chain + container snapshots

cfs-log           # pretty-print the snapshotter's log
cfs-log-follow    # follow it live
```

See [docs/devcontainer.md](docs/devcontainer.md) for the full workflow —
version knobs, startup details, cleanup — and
[docs/running.md](docs/running.md) for configuring a general host.

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md) first. All commits must be signed off under
the [Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`), and pull requests must be opened and reviewed by a human.
Automated coding agents must additionally follow [AGENTS.md](AGENTS.md).

## License

cfs-snapshotter is licensed under the [Apache License 2.0](LICENSE).

Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
