# CI and release validation — 2026-09-07

Implementation: [PR #424](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/pull/424),
based on `main` at `36cd5ba`. This is dated validation evidence, not a release
announcement or a promise of future CI timings.

## Regression and functional checks

- The new path-filter regression fails against the original workflow, including
  `tools/release/verify-tag-on-main.sh`, and passes against the corrected filters.
- The required-check script is executed with successful, failed, cancelled,
  missing and unexpectedly skipped job results. Incorrect states fail closed.
- Publication tests cover numeric version ordering, newer drafts/published
  releases/tags, invalid versions, unavailable APIs, failed Docker promotion,
  missing drafts and attempts to replace public releases. External publication
  commands are mocked in these tests.
- Full Go tests, race tests, coverage tests, vet, build, Staticcheck and Govulncheck
  pass with Go 1.26.6. Local and Linux CI coverage are both 73.9% (minimum 73%).
- Actionlint, 170 Node unit tests and npm audit pass; npm reports zero findings.
- Docker smoke passes for Linux amd64 and arm64, including initial account
  creation, PID 1 running as `app`, and login after recreating the container as
  `app` with the original volume. Both architectures also pass after the runtime
  package update described below.
- The archive-building step was executed from the workflow in an isolated source
  checkout: all five archives were produced and checked. The packaged Linux
  amd64 binary was started under Linux from its extracted archive. No GitHub
  Release or GHCR package was published by these validation commands.

## Go comparison

Local macOS arm64, Go 1.26.6, same checkout and Go cache. One initial run of each
mode warms the cache and is excluded; three subsequent samples are retained.
Modes alternate order between repetitions. Times include the entire command.

| Mode | Warm samples (seconds) | Median |
| --- | --- | --- |
| `go test -race -count=1 ./...` | 60.62, 60.71, 60.76 | 60.71 s |
| `go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...` | 29.24, 29.14, 29.12 | 29.14 s |
| `go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...` | 62.09, 61.50, 59.66 | 61.50 s |

Every coverage sample reports 73.9%. No source uses a race-specific build tag.
The combined mode reduces sequential work in this local comparison but does not
shorten the parallel CI test critical path. The matrix remains separate. These
macOS measurements do not establish a Linux release-runner speedup, so release
also retains separate instrumentation while removing its redundant plain pass.

## Docker comparison

A single paired comparison built the builder stage for Linux amd64 on the same
Docker Desktop Linux arm64 worker, using the original and updated Dockerfiles.
The updated builder's Go tool reports `GOHOSTARCH=arm64` and cross-compiles amd64.

| Builder | Go compilation step | Total builder build/export |
| --- | --- | --- |
| Original, emulated amd64 | 66.9 s | 115.88 s |
| Native builder, cross-compiling amd64 | 41.6 s | 60.51 s |

This is one sample per mode, not a median. Total times include differing image
setup/export costs and must not be treated as projected release savings.
Both methods produced identical binary SHA-256 values:

- `webserver`: `ab13edf21cc730db10c06ac1901af2c7cac4a0c3482edf647be441b9514ec608`
- `persistence-owner`: `c06a60b6b97391de221ac06fca10fb392c51b2515d15b4abbab916af2390d984`

## Runtime security scan

Trivy 0.74.0, pinned by digest, initially reported CVE-2026-14456 in installed
`libcrypto3` and `libssl3` 3.5.7-r0 (two HIGH findings). Updating installed runtime
packages upgrades both to 3.5.8-r0. The rebuilt amd64 image has zero HIGH/CRITICAL
OS-package findings with the same scanner, without suppressions or exclusions
for unfixed vulnerabilities. This inventory finding does not establish that the
application exposes the vulnerable OpenSSL operation.

## GitHub evidence

The [first CI run](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/34139731147)
passed in 4 min 04 s. Docker smoke took 57 s and E2E took 3 min 44 s. Managed
CodeQL analyses for Go, JavaScript/TypeScript and GitHub Actions also passed.
This single run is functional evidence, not a before/after performance median.

The E2E log confirms browser installation and cache saving under
`/home/runner/work/_temp/ms-playwright`, matching the cache action's path.
Both coverage and Playwright diagnostic artifacts were uploaded successfully.
The final PR additionally gives artifacts attempt-specific names so reruns keep
earlier diagnostics. Final-head status and subsequent cache evidence are tracked
on the PR.
