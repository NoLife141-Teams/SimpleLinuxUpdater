# CI and release validation

`ci-required` remains the required merge check. Preflight combines toolchain
alignment and path selection; frontend quality combines npm audit and Node unit
tests. Go tests, Go quality, Playwright and Docker must succeed when selected,
and must be skipped otherwise. Pushes to `main` run all checks. CodeQL remains
a separate GitHub protection.

The Go filter includes scripts, templates, static resources and documentation
inspected by architecture tests. Docker validation builds amd64 without registry
credentials, checks PID 1 runs as `app`, creates a test account, and recreates the
container explicitly as `app` before verifying account persistence. It removes
only its own temporary container and volume.

Playwright and the cache action share an absolute directory under `RUNNER_TEMP`.
OS dependencies are installed each time with shared APT retry settings. Coverage
and Playwright reports/traces are retained for seven days, including failed runs.
Temporary application databases are excluded. Playwright keeps one CI worker.

## Publication sequence

The unprivileged `Release Tag Signal` remains separate. The release workflow is
loaded from the default branch. Every job independently verifies signal identity,
tag SHA and main ancestry before checking out the release commit. Publication
helpers are preserved from the trusted default-branch checkout before switching
to the tag, so older tags cannot replace the current policy.

1. `release-gate` validates the exact tagged sources, race tests, coverage (73%),
   Go quality/security, frontend unit tests, npm security, Playwright and version
   metadata. The redundant uninstrumented Go test pass is removed.
2. `publish-release` builds all five archives, checks checksums and archive
   integrity, starts the packaged Linux amd64 server, and creates or updates a
   **draft** GitHub Release. Already public releases cannot be replaced.
3. `publish-docker` pushes the versioned image, then tests its returned digest on
   amd64 and arm64. Only after both startup/persistence tests succeed does it
   consider `latest` and publish the GitHub Release.

Publication is serialized without cancelling a running publication. GitHub
concurrency retains at most one pending run; if several tags arrive together,
a displaced pending version must be retriggered.

`latest` advances only if no greater numeric stable `vX.Y.Z` version exists among
GitHub Releases (including drafts) and tags reachable from freshly fetched
`main`. Drafts and queued tags reserve their versions. Retrying `v0.4.9` after
`v0.4.10` exists can publish its own version but cannot take `latest`. Prereleases
do not participate. GitHub latest follows the same decision. API/Git errors stop
publication rather than assuming no newer version exists.

GitHub and GHCR have no shared transaction. A failure may leave a draft and a
versioned image; the public GitHub announcement remains deferred. If GHCR
promotion succeeds but GitHub finalization fails, retry the same unpublished
version. Its reserved draft prevents older attempts from regressing `latest`.
A public release is immutable in this workflow; fixes require a new version.

## Performance and security

The Docker builder runs on `BUILDPLATFORM`, cross-compiling both executables with
`TARGETOS` and `TARGETARCH`. Runtime package installation and smoke tests still
execute on each target architecture. Toolchain alignment checks the new builder
form against the exact Go version in `go.mod`.

Race and coverage remain separate CI matrix entries. Combining them requires
comparable timing and coverage measurements; fewer commands alone do not imply
a shorter parallel CI critical path.

`Security Audit` runs Govulncheck, npm audit and an OS-package scan of a freshly
built runtime image weekly, and supports manual runs. Trivy is pinned by digest
and fails on HIGH/CRITICAL OS vulnerabilities, including those without a fix.
It has read-only repository permissions and does not run E2E or publish packages.
Dependabot and managed CodeQL retain their existing responsibilities.
