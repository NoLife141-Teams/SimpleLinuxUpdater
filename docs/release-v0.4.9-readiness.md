# v0.4.9 Release Readiness

Pre-publication preparation only. Merge, tag creation and publication require
separate explicit authorization; production deployment is outside this task.

## Candidate

- Prepared: 2026-09-07 America/Toronto
- Version: `v0.4.9`; previous published release: `v0.4.8`
- Base: `12d86d5af91d12804cda269cfafa72441c6a90f2`
- Branch: `release/prepare-v0.4.9`
- No version tag, GitHub draft/release or registry image has been published.

## Scope

The complete range after `v0.4.8` includes PRs #408–#426 that were merged into
this base: SSE lifetime, canonical IP endpoints, notification destination
hardening, cooperative maintenance shutdown, recoverable maintenance release,
Docker ownership repair, stale refresh retries, durable scheduler recovery,
rollout-origin/restore-timezone fixes, stable server rows, and CI/publication
qualification. Test-only SQLite and UI stabilization changes are included too.
See the [changelog](../CHANGELOG.md) for user-visible behavior and compatibility.

## Qualification

Local checks used Go `1.26.6` from `go.mod` and the existing CI commands:

| Check | Observed result |
| --- | --- |
| Go tests and race detector | Passed (`-count=1`) |
| Atomic Go coverage / existing threshold script | 73.8%; minimum remains 73.0% |
| `go vet`, build, Staticcheck, Actionlint | Passed |
| Govulncheck | Passed; no affected calls or imported packages; one required-module finding without a called vulnerable symbol |
| Publication Python helpers | 30 tests passed with mocked services; no publication |
| Frontend unit tests / npm audit | 170 tests passed; zero npm vulnerabilities |
| Workflow release metadata step | Passed with `RELEASE_TAG=v0.4.9`, without creating a tag |
| Five release archives | Built with the workflow commands; SHA-256 and archive integrity passed |
| `tools/release/verify-archives.sh` | Passed under Linux amd64, including execution of the packaged binary and `/login` response |
| Docker OS and Go binary scans | Both architectures passed, zero HIGH/CRITICAL findings with the pinned Trivy 0.74.0 scanner |
| Docker startup / non-root / persistence smokes | Local execution pending approval of the existing script's temporary-container cleanup |
| Playwright E2E and PR checks | To be verified on the preparation PR |

Archive targets are Linux amd64/arm64, macOS amd64/arm64 and Windows amd64.
Only the Linux amd64 archive was executed. The other four were cross-compiled
and checked for archive integrity and checksums, not executed.

Docker builds used `--pull --no-cache-filter runtime --load` separately for
`linux/amd64` and `linux/arm64`; builder cache remained available. The local
Docker image IDs below were passed directly to the scans:

| Platform | Local image ID | Installed `libcrypto3` / `libssl3` |
| --- | --- | --- |
| linux/amd64 | `sha256:d9ea250b4a2a490b38d648f0e00d7948e69a3a1143509d3dda9614513b8b02a1` | `3.5.8-r0` / `3.5.8-r0` |
| linux/arm64 | `sha256:9f98f053eac61d9a2db583a4f0fe83e34a3478cd8ef407956136cecee60abe59` | `3.5.8-r0` / `3.5.8-r0` |

These are separate local OCI indexes produced by BuildKit, not an uploaded
multiarchitecture release digest. Scan reports included Alpine OS packages,
`app/webserver` and the Go persistence-owner helper. Results describe the
scanner database at qualification time, not an absence of all vulnerabilities.
All execution used isolated test data and containers.

## Previously distributed image

A read-only registry lookup on 2026-09-07 still resolved `latest` to
`sha256:66d007ce2e056d8552b9e7aebc12a8fe1946ba0058fa132a58379de244d75f6a`.
The [latest observed Security Audit](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/34150336086)
scanned that same digest and failed on both architectures: `libcrypto3` and
`libssl3` `3.5.7-r0`, HIGH `CVE-2026-14456`, reported fixed in `3.5.8-r0`.
This package finding does not establish application exploitability. The passing
local rebuilds above do not change the image currently distributed by GHCR.

## Publication still required

- Revalidate availability and merge the preparation PR only after authorization.
- Verify CI and CodeQL on the exact merged `main` commit before tagging it.
- Run the existing tag-signal/release chain; qualify the uploaded candidate digest.
- Verify all downloaded archives/checksums, `publication.json`, official index and
  child platforms, eligible `latest` promotion, and a distributed-image audit.

Local archives and images are preparation evidence, not artifacts already
published or guaranteed to be byte-identical to a later release build.
