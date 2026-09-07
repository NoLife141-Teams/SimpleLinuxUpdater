# Multiarchitecture image-store regression — 2026-09-07

The [v0.4.9 release attempt](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/34156958781)
passed its source gate, archive validation, amd64 runtime/persistence smoke and
amd64 OS/Go scan. Loading arm64 then failed with `cannot overwrite digest` before
the application started. The runner used Docker 28.0.4 with `overlay2`.

The attempt created a draft with five archives and checksums, plus the candidate
`candidate-v0.4.9-34156958781-1` at
`sha256:a9c28712691252d2ef45c2855f10be2bed82afa3b64d4d5c15ba15a150d0e12e`.
It did not create `publication.json`, the official v0.4.9 image or a public
release. `latest` and v0.4.8 remained at
`sha256:66d007ce2e056d8552b9e7aebc12a8fe1946ba0058fa132a58379de244d75f6a`.

## Reproduction and correction

Two isolated Docker-in-Docker daemons used the same pinned Docker 28.0.4 image:
`docker:28.0.4-dind@sha256:4de6a5668da5d138bf9c18b29d2702e43c8773daee085087ba4fac61a6d34b7f`.
No host Docker socket, production data or published API port was mounted.

The reduced reproducer pulls the candidate above by its index digest, first
with `--platform linux/amd64`, then with `--platform linux/arm64`:

- `dockerd --storage-driver=overlay2`: amd64 succeeds; arm64 fails with the
  release's exact error. Repeating the arm64 pull fails identically.
- Same Docker version with `--feature containerd-snapshotter=true`: both pulls
  succeed, and image inspection still reports the original index digest.

This isolates the image store from application startup, emulation and package
scanning. The symptom also matches [Moby issue #43188](https://github.com/moby/moby/issues/43188).
[Docker's GitHub Actions documentation](https://docs.docker.com/build/ci/github-actions/multi-platform/)
requires the containerd image store to load multi-platform images locally.

The correction configures that store before QEMU/Buildx in publication, retaining
Docker 28.0.4 to match the reproduced runner version. CI now builds and loads both
application architectures locally and runs the existing smokes against one
immutable image ID. No publication-policy, runtime payload or tag changes are
part of this correction.

## Regression coverage

The new workflow tests failed against the previous configuration and pass with
the correction. They parse the actual daemon configuration, require setup before
Docker use, execute the CI smoke loop with isolated command adapters, and verify
that both architectures receive the same image ID and arm64 failure propagates.
Real Docker pull reproduction is separate from those adapter tests. PR CI must
also pass the actual amd64 and arm64 application smokes before this correction
is merged; a passing PR is not evidence of a resumed release publication.
