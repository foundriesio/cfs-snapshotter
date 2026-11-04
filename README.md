# cfs-snapshotter

A [containerd](https://containerd.io/) remote (proxy) snapshotter that serves
container rootfs from content-addressed, composefs-backed storage — using
**composefs + EROFS + fs-verity** to mount a container's filesystem with a single
`mount` syscall instead of unpacking and stacking layers.

> **Status: early development.** The snapshotter serves real image content
> from a local [ocicfs](https://github.com/foundriesio/ocicfs) store: `docker
> run` mounts the image's EROFS metadata once per image and gives each
> container one flat overlay whose data comes from the store's
> fs-verity-backed objects. By default a plain `docker pull` fills the
> store: containerd fetches the layers as it always would, the snapshotter
> stages them, and the whole image is built into the store when the last
> layer is committed. A machine started with `-allow-unstaged-pulls=false`
> instead admits only images put in the store out of band — `ocicfs pull`,
> pinned by digest — and declines every other pull; the pulls it admits
> download no layers (`ctr` and `docker` still fetch the image metadata:
> index, manifest, config, etc.). Either way a container gets the same
> mounts. For offline use, staged mode needs only `docker load`; out-of-band
> mode needs `ocicfs pull --from-tar` first. See the
> [offline loading guide](docs/running.md#loading-images-offline-docker-load).

## Install or build

Released binaries and .deb packages: see [INSTALL.md](INSTALL.md). To build
from a checkout:

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
                  # (CFS_ALLOW_UNSTAGED_PULLS=0 selects the out-of-band mode;
                  # unset, it restarts in the default staged mode)
make check        # lint, vet, unit tests
make test         # unit tests
make test-e2e     # end to end, both work modes: out-of-band zero-fetch
                  # and staged cold pull
```

Where each target runs:

| Target | Dev host | Devcontainer |
|---|---|---|
| `make`, `build`, `fmt`, `clean` | yes | yes |
| `install` | yes, as root or with a writable `PREFIX` or `DESTDIR` | yes |
| `check`, `test`, `vet`, `lint` | yes, with Go and golangci-lint installed; tests that need root, fs-verity or mkcomposefs skip themselves | yes, as in CI |
| `run-cfs`, `test-e2e` | no: needs the devcontainer's runtime stack and registry | yes; `test-e2e` needs a container started with `./devshell.sh` (an editor-opened one lacks the ocicfs CLI) |
| `root-test` | as real root only; same failures as in the container | runs, but all subtests currently fail; see [Conformance Suite](docs/devcontainer.md#conformance-suite) |
| `deb`, `deb-lint`, `deb-test` | yes, with Go and Docker: the package builds in its own container | not needed |

To test manually, drive the snapshotter with plain `docker` and `ctr`
commands — both are preconfigured to use cfs. The devcontainer boots in the
default staged mode, so a `docker pull` of any seeded image just works. The
walkthrough below shows the out-of-band mode instead: switch the daemon,
seed the local registry with a runnable test image, and stage it:

```sh
CFS_ALLOW_UNSTAGED_PULLS=0 make run-cfs   # switch to the out-of-band mode

layout=$(mktemp -d)
SEED_RUNNABLE=1 SEED_KEEP_LAYOUT="${layout}" \
    .devcontainer/scripts/seed-registry.sh 127.0.0.1:5000/cfs/demo:latest
digest=$(jq -r '.manifests[0].digest' "${layout}/index.json")

ocicfs pull --from-dir "${layout}" "cfs/demo@${digest}"

docker pull "127.0.0.1:5000/cfs/demo@${digest}"   # zero layer downloads
docker run --rm "127.0.0.1:5000/cfs/demo@${digest}"
ctr snapshots ls                  # committed chain + container snapshots

cfs-log           # pretty-print the snapshotter's log
cfs-log-follow    # follow it live
```

The pull downloads no layers — only the image metadata (index, manifest,
config, etc.) is fetched — and the container runs from the ocicfs store's
content. Running an image that is not in the store fails with an error
telling you to pull it by digest out of band.

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
