<!-- SPDX-License-Identifier: Apache-2.0 -->

# Releasing cfs-snapshotter

Maintainer-facing policy and procedure. The release pipeline is automated:
pushing a version tag builds, validates, and publishes the release; the
maintainer's job is choosing the version, writing the summary, and tagging.

## Versioning

- Releases are semver tags: `vX.Y.Z`. Pre-1.0, the compatibility surface —
  daemon flags, the snapshotter's on-disk layout, the admission behavior —
  may still change in minor releases; every such change must be called out
  in the release notes.
- Release candidates are `vX.Y.Z-rc.N` and publish GitHub **pre-releases**
  (Debian version `X.Y.Z~rc.N`, sorting before the final). The first
  release of a series should go through at least one rc: the rc run
  exercises the entire pipeline end to end without claiming "latest".
  Because GitHub strips `~` from asset filenames, rc `.deb` assets are
  published — and checksummed — with `~` replaced by `.`; the version
  inside the package keeps the `~`.

## Required checks

- Tag only commits on `main` whose CI run is green.
- The release workflow independently rebuilds and gates the tagged
  revision per architecture: build, unit tests, the devcontainer e2e
  suite, the release validation script (ELF architecture, static linkage,
  stamped version and revision, archive layout), and the deb suite
  (`deb`, lintian, install/upgrade/remove/purge tests). A release is
  created only from green builds.

## Cutting a release

1. Write the release summary (template below).
2. Create an **annotated** tag whose message is that summary, and push it.
   Signing the tag is recommended:

   ```sh
   git tag -s --cleanup=whitespace vX.Y.Z -F notes.md   # or: git tag -a ...
   git push origin vX.Y.Z
   ```

   `--cleanup=whitespace` keeps the `## ` headings, which the default
   cleanup strips as comments.

3. The workflow builds both architectures, validates everything, uploads
   all artifacts to a **draft** release, and finalizes it as the last
   step. `-rc.N` tags are marked pre-release.
4. Verify the release page: two `.tar.gz` archives, two `.deb` packages,
   `checksums.txt` listing every artifact including the SBOMs, the source
   archives, and notes consisting of your tag message (plus the generated
   pull-request list when a previous stable release exists).
5. Remove the Dependabot entries from the generated list, and the "New
   Contributors" line for Dependabot, with `gh release edit vX.Y.Z
   --notes-file <file>`.

## Tag message template

Write one line per paragraph and per bullet, without hard wrapping: the
release page renders every newline as a line break.

```
<project> X.Y.Z — <short summary>

## Highlights

- <the two or three changes a user cares about>

## Breaking changes

- <changed daemon flags / on-disk layout / admission behavior, or "None since v<previous>">

## Upgrade notes

- <changed defaults, containerd config changes, required restarts, or "None">

## Compatibility

- Kernel: Linux 6.6+ (EROFS, fs-verity, overlayfs data-only lowers)
- ocicfs: built against <version>
- containerd: <supported range / config notes>
- Tested on: <distros / devices>

## Known issues

- <omit the section when there are none>
```

The very first release (`v0.1.0`) is introduction-style: what the
snapshotter is, the two work modes, the compatibility floor, known
limitations — no change list, since there is nothing to diff against.

## After publication

- **Never move or delete a release tag; never replace a published
  artifact.** Published bytes are immutable.
- If a job fails before the draft is finalized, re-run the failed jobs.
  **Never re-run the whole workflow for an already-published release** — a
  rebuild produces different bytes than the published checksums attest.
- A defective release gets a **patch release** with the fix; optionally
  edit the defective release's notes to add a Known-issues warning. It
  stays published: users depend on old releases for reproducible installs
  and rollback.
- All releases are retained. Deprecating a release line is announced in
  the notes of the release that supersedes it.
