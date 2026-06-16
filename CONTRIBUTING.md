# Contributing to cfs-snapshotter

Thanks for your interest in contributing! cfs-snapshotter is developed in the
open and aims to follow the conventions of the
[containerd](https://github.com/containerd/containerd) community, with the goal
of donation to the containerd project. Please also read the containerd project's
[general contribution guidelines](https://github.com/containerd/project/blob/main/CONTRIBUTING.md),
which this guide builds on.

By participating, you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

- For anything beyond a small, obvious fix, **open an issue first** and discuss
  the approach. Large or high-impact pull requests submitted without prior
  discussion are likely to be closed — consensus on design before code keeps
  reviews fast and avoids wasted work.
- Keep each pull request focused on a single change. Submit independent changes as
  separate pull requests.

## Developer Certificate of Origin (DCO)

All commits **must** be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/). Sign off by
adding a `Signed-off-by` trailer with your real name and email:

```
Signed-off-by: Jane Developer <jane@example.com>
```

The easiest way is to pass `-s` when committing:

```bash
git commit -s -m "snapshotter: short imperative subject"
```

The sign-off certifies that you wrote the patch or otherwise have the right to
submit it under the project's open source license. Commits without a valid
sign-off cannot be merged.

## Automated and AI-generated contributions

This project follows containerd's policy:

- **Pull requests must be opened by a human.** Any use of automation to create
  pull requests must be approved by a maintainer in advance. Accounts opening PRs
  via automation without prior approval may be restricted from the project.
- **All AI-generated or AI-assisted content** — code, commit messages, PR
  descriptions, or comments — **must be reviewed by the human author before it is
  submitted.** The human author is responsible for the correctness and quality of
  everything in their pull request.

If you use a coding agent in this repository, it must also follow the rules in
[AGENTS.md](AGENTS.md).

## Submitting a pull request

1. Read the [Code of Conduct](CODE_OF_CONDUCT.md) and [LICENSE](LICENSE).
2. [Fork](../../fork) and clone the repository. Note that the module pins
   `replace github.com/knarr-project/knarr => ../knarr`, so check out
   [knarr](https://github.com/knarr-project/knarr) **beside** this repository.
3. Create a branch off `main`:
   ```bash
   git checkout -b my-change main
   ```
4. Make your change, add tests, and make sure everything passes (see below).
5. Commit with a sign-off and a clear, imperative commit message (see
   [How to write a Git commit message](https://cbea.ms/git-commit/)). Each commit
   should build and tell a coherent story.
6. Keep your branch current:
   ```bash
   git pull --rebase upstream main
   ```
7. Push to your fork and [open a pull request](../../pulls) against `main`.

## Code style and checks

- Go code uses standard formatting. Run `gofmt` (CI rejects unformatted code) and
  `go vet ./...`.
- Run the relevant tests before submitting:
  ```bash
  make vet
  make test       # unit tests
  make build
  ```
- Changes that touch the snapshotter's privileged paths must pass the containerd
  conformance suite, which requires root:
  ```bash
  sudo -E PATH=$PATH make root-test   # or: podman unshare make root-test
  ```
- Every Go source file carries the Apache-2.0 SPDX header
  (`// SPDX-License-Identifier: Apache-2.0`).

## Continuous integration

CI runs `gofmt`, `go vet`, `make build`, the unit tests, and the privileged
snapshotter conformance suite. A pull request is expected to be green before
review.

## Reporting security issues

Please do **not** report security vulnerabilities through public issues or pull
requests. See [SECURITY.md](SECURITY.md) for the coordinated-disclosure process.
