# v0.4.10 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-09-07 America/Toronto.
- Previous published version: `v0.4.9`.
- Implementation base: `973a715495f6688b4bb81112a81399dbfcbfe1a9` on `main`.
- Preparation branch: `codex/prepare-v0.4.10`.
- Includes PRs #428–#431 after the v0.4.9 tag: container qualification and release
  credential isolation, job/action ownership, SSH outcome safety, session
  revocation, approval revalidation and identity, host-fact freshness, bounded
  live output with persisted logs, and recoverable backup maintenance.
- Release changes are metadata and documentation only; the complete user-facing
  changes and upgrade instructions are in [CHANGELOG](../CHANGELOG.md).

## Upgrade requirements

Existing non-root SSH targets need **Enable apt** once to install the updated
helper. Outdated helpers fail closed. API clients must send the displayed
`job_id` and `approval_generation` for approval/cancellation and must not replace
those identities with newer values after the operator has confirmed an old plan.

## Qualification

| Check | Result |
| --- | --- |
| Exact Go toolchain | Go 1.26.6 from `go.mod`, selected with `GOTOOLCHAIN` locally |
| Full uncached race and atomic coverage suites | Passed; 74.1% aggregate coverage, minimum remains 73.0% |
| Vet, Staticcheck, Govulncheck, Actionlint and native build | Passed; no called or imported-package vulnerabilities; one required-module finding without affected calls |
| Frontend unit tests / npm audit | 171 passed; zero npm vulnerabilities |
| Playwright browser tests | 68 passed with one worker |
| Workflow release metadata step | Passed with `RELEASE_TAG=v0.4.10`, without a tag |
| Five workflow-format release archives | Built with Go 1.26.6; checksums and archive integrity passed |
| Exact packaged Linux amd64 binary | `verify-archives.sh` passed on Linux amd64, including `/login` |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Both passed with pinned Trivy 0.74.0; zero HIGH/CRITICAL findings |
| Official v0.4.9 database upgrade | Passed; existing account and encrypted inventory preserved |
| Live SSH, scheduled scan and isolated restore | Passed with the container systemd limitation documented below |
| Hosted release-tag protection | Active `Protect release tags v*`, matching `refs/tags/v*`, blocking updates/deletions |

Local Docker qualification used `--pull --no-cache-filter runtime` and the same
loaded multiarchitecture image ID for both platforms:
`sha256:f3966bebd1f2eec0f9ebeb414f37aab7003833491ed7ef7346b416fb678250a3`.
It was not pushed to GHCR. Scan results describe the vulnerability database at
qualification time. The other four archives were cross-compiled and checked for
integrity, not executed. Local preparation artifacts are not publication bytes;
the trusted release workflow must qualify its own final artifacts.

See [release smoke results](release-v0.4.10-smoke.md) for disposable-host and
backup validation, including explicit environment limitations.

## Before tagging

1. Review and merge this preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.10`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
archives/checksums, `publication.json`, both image platforms, version/latest
digest identity and the distributed-image audit after publication. Do not reuse
local preparation artifacts as proof that those future steps have passed.
