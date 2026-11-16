# github releases

status: implemented (`.github/workflows/release.yml`).

## what a release is

a release tag `v$VERSION` gets a github release with the native pats build (every driver in, no build tags) as one
tarball per platform, and a `checksums.txt` of their sha256 sums:

- `pats_<version>_linux_amd64.tar.gz`
- `pats_<version>_linux_arm64.tar.gz`
- `pats_<version>_darwin_amd64.tar.gz`
- `pats_<version>_darwin_arm64.tar.gz`

each holds `pats`, `LICENSE` and `README.md` under a `pats_<version>_<os>_<arch>/` dir. the binaries are static
(`CGO_ENABLED=0`), built with `-trimpath`, and not upx-compressed. the darwin ones are not signed or notarised, so a
browser download is quarantined by gatekeeper. `curl` is not.

`make dist` builds the same tarballs locally into `./dist`.

the github release is independent of the snap (`docs/spec/snap.md`) and the images (`publish_images.yml`). the same
tag starts all three, and none waits on the others.

## the workflow

on a `v*` tag push:

1. `version` -- the tag must be `v$(cat VERSION)`, or the binaries would report another version.
2. `checks` -- build, lint and test, the native build and the strict one (`.github/workflows/checks.yml`, the same
   reusable workflow `lint_and_test` calls on every change).
3. `dist` -- `make dist`, then the linux/amd64 binary's `--version` must be the tag's version from a clean tree.
4. `release` -- a draft release, the assets uploaded into it, then published. a failed upload leaves a draft, never a
   half-done public release. a re-run finds the draft and uploads into it with `--clobber`.

a version with a suffix (`1.0.0-beta.3`) is a pre-release. the notes are github's generated ones, from the pull
requests since the previous `v*` tag, since most older tags have no release to start from.

## cutting a release

1. a `release: vX.Y.Z` pull request bumps `VERSION`. it runs the whole snap suite (`snap.yml`).
2. once merged, tag the merge commit `vX.Y.Z` and push the tag.

tags up to `v1.0.1` were cut before this workflow. they have no tarballs and get none.
