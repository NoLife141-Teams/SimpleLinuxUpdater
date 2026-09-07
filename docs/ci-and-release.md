# CI and release validation

`ci-required` remains the required merge check. Preflight combines toolchain
alignment and path selection; frontend quality combines npm audit and Node unit
tests. Go tests, Go quality, Playwright and Docker must succeed when selected,
and must be skipped otherwise. Pushes to `main` run all checks. CodeQL remains
a separate GitHub protection.
The helper regression tests use Bash and Python 3 on Linux/macOS and skip on
Windows, matching the existing release-lineage script tests.

The Go filter includes scripts, templates, static resources and documentation
inspected by architecture tests. Docker validation builds amd64 without registry
credentials, checks PID 1 runs as `app`, creates a test account, and recreates the
container explicitly as `app` before verifying account persistence. It removes
only its own temporary container and volume.

Playwright and the cache action share an absolute directory under `RUNNER_TEMP`.
OS dependencies are installed each time with shared APT retry settings. Coverage
and Playwright reports/traces are retained for seven days, including failed runs.
Artifact names include the attempt number so retries preserve earlier diagnostics.
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
3. `publish-docker` reads the release and GHCR state **before any registry write**.
   A new build pushes only `candidate-vX.Y.Z-RUN_ID-ATTEMPT`; a recorded retry
   reuses its digest without rebuilding. Each architecture passes startup,
   non-root/persistence checks and an OS + Go binary vulnerability scan.
4. The qualified version, commit, image digest, architectures, checks and run
   identity are uploaded as `publication.json` to the draft, without clobbering.
   Only then is that same digest assigned to `vX.Y.Z`, conditionally to `latest`,
   and finally announced by publishing the GitHub Release.

Candidate references in a public registry are publicly accessible but are not
announced as validated versions. No rebuild occurs between qualification and
promotion. HTTP/authentication failures are fatal; only an explicit manifest
404 is treated as an absent tag. An existing official version with a conflicting
or missing publication record is never overwritten.

Publication is serialized without cancelling a running publication. GitHub
concurrency retains at most one pending run; if several tags arrive together,
a displaced pending version must be retriggered.

`latest` advances only if no greater numeric stable `vX.Y.Z` version exists among
GitHub Releases (including drafts) and tags reachable from freshly fetched
`main`. Drafts and queued tags reserve their versions. Retrying `v0.4.9` after
`v0.4.10` exists can publish its own version but cannot take `latest`. Prereleases
do not participate. GitHub latest follows the same decision. API/Git errors stop
publication rather than assuming no newer version exists.

GitHub and GHCR have no shared transaction. If promotion or finalization fails,
**rerun only `publish-docker`**. The durable record recovers the same digest even
when a write succeeded but its response was lost. A draft retry requalifies that
digest against current vulnerability data and skips matching official tags.
Once the release is public and its version digest matches, the retry is a no-op.
The archive job refuses replacement after qualification has frozen the release.
A public release without a record (including legacy releases) is rejected; fixes
require a new version. Do not delete the record to force a rebuild.

These checks assume this workflow is the sole writer of release assets and
version/latest tags. External administrator writes are not covered by the
workflow concurrency lock. Conflicting recorded versions fail closed.

Docker logs and filtered container state are captured before cleanup, and the
archive server's stdout/stderr is retained outside its temporary extraction.
Failed CI/release qualifications upload those diagnostics for seven days, with
attempt-specific names. HTTP probes have connection and total time limits.
Temporary databases and full container configuration are not uploaded.

## Performance and security

The Docker builder runs on `BUILDPLATFORM`, cross-compiling both executables with
`TARGETOS` and `TARGETARCH`. Runtime package installation and smoke tests still
execute on each target architecture. Toolchain alignment checks the new builder
form against the exact Go version in `go.mod`.
The named `runtime` stage upgrades Alpine packages before installing runtime
dependencies. Release builds use `pull: true` and `no-cache-filters: runtime`,
forcing those package commands to run while preserving the Go builder cache.
APK `--no-cache` alone does not invalidate BuildKit layers.
The QEMU action and its nested binfmt image are both pinned; only arm64 emulation
is installed. Image caching in the QEMU action is disabled so the daemon fetches
the content-addressed image rather than restoring a separate image tar cache.

Race and coverage remain separate CI matrix entries. Combining them requires
comparable timing and coverage measurements; fewer commands alone do not imply
a shorter parallel CI critical path.

`Security Audit` runs Govulncheck and npm audit weekly and supports manual runs.
It resolves the currently distributed GHCR `latest` digest once, then scans that
exact digest for amd64 and arm64 without rebuilding or executing the application.
Trivy is pinned by digest and fails on HIGH/CRITICAL OS and Go binary dependency
vulnerabilities, including those without a fix. A scan also fails if either the
OS packages or the application Go binary was not detected. The same scan is a
mandatory release qualification. JSON findings and scanner stderr survive failures.
The audit has read-only repository/registry permissions and does not publish.
Dependabot and managed CodeQL retain their existing responsibilities.
