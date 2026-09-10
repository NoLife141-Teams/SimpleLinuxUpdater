# v0.4.11 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-09-09 America/Toronto.
- Previous published version: `v0.4.10`.
- Implementation base: `5ffb011d907138283ba734b49dc11f1503d86ec6` on `main`.
- Preparation branch: `codex/prepare-v0.4.11`.
- Includes PRs #433–#439: authentication/session safety, delayed rollout and
  canary gates, wave timing, encoded inventory names, retained audit facts,
  serialized timezone changes, dependency updates, and server availability.
- Disposable-host qualification exposed a policy-preview inconsistency:
  disabled servers were still counted as matches and assigned rollout stages.
  Commit `cd9fd9e` fixes the separate preview exclusion path, adds the
  `server disabled` UI reason, and includes Go/browser regressions.
- README, UI version, installation/deployment/configuration examples, changelog,
  and the reusable release-smoke checklist are updated.

## Upgrade requirements

Existing servers remain enabled. Availability changes preserve credentials and
history and survive persistence/restart. An upgrade from v0.4.10 does not require
another SSH helper installation or a new API approval contract. Installations
upgrading from older versions must also follow the v0.4.10 upgrade notes.

## Qualification

| Check | Result |
| --- | --- |
| Exact Go toolchain | Go 1.26.6 from `go.mod`, selected with `GOTOOLCHAIN` locally |
| Full uncached race and atomic coverage suites | Passed; 74.3% aggregate coverage, minimum remains 73.0% |
| Vet, Staticcheck, Govulncheck, Actionlint and native build | Passed; no called or imported-package vulnerabilities; one required-module finding without affected calls |
| Frontend unit tests / npm audit | 177 passed; zero npm vulnerabilities |
| Playwright browser tests | 71 passed; the policy-preview exclusion reason is included |
| Workflow release metadata step | Passed with `RELEASE_TAG=v0.4.11`, without creating a tag |
| Five workflow-format release archives | Passed; all five binaries built with Go 1.26.6, archive integrity and SHA-256 checksums verified |
| Exact packaged Linux amd64 binary | Passed; unmodified verify-archives.sh ran in Linux amd64 and the packaged binary served /login |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Both passed with pinned Trivy 0.74.0; zero HIGH/CRITICAL findings |
| Official v0.4.10 database upgrade | Passed; account, encrypted password/key, inventory, host trust, policies, jobs and audit history preserved; existing servers enabled |
| Live SSH and package update | Passed using password and private-key authentication and the existing v0.4.10 helper |
| Two-host scheduled maintenance | Passed; successful delayed wave, disabled-host exclusion, and failed-canary gate after disabling the canary |
| Disabled state, automatic refresh and isolated restore | Passed; restart persistence, automatic skip/resume, encrypted backup verification, restored credentials/history and session invalidation |
| Hosted release-tag protection | Active `Protect release tags v*`, matching `refs/tags/v*`, blocking updates/deletions |

The official v0.4.10 macOS arm64 archive was verified against its published
checksum before use:
`0ac29773dc5825f3e66ad88273d6976856ce163953e97f1996d8e2db0c7a44ff`.

Local Docker qualification used `--pull --no-cache-filter runtime` and the same
loaded multiarchitecture image ID for both platforms:
`sha256:ad36975466ce0eb3e2a4513f2e535125a78c8e4c5f455aab6b90d454a2d7a85f`.
It was not pushed to GHCR. The runtime smoke used the repository's startup,
non-root and persisted-login checks with graceful container cleanup. Scan
results describe the vulnerability database at qualification time.
The other four archives were checked for integrity, not executed.
Local preparation artifacts are not publication bytes; the trusted release
workflow must qualify its own final artifacts.

See [release smoke results](release-v0.4.11-smoke.md) for live behavior and the
explicit container/systemd limitations.

The first two PR coverage attempts hit one-second setup deadlines in different
shutdown tests, before cancellation began. The lifecycle fixtures now share an
isolated, single-connection in-memory SQLite helper to remove filesystem sync
latency while retaining the real SQL repository. Both affected tests passed 100
repetitions. Separate negative controls that ignore cancellation still fail at
the unchanged one-second cancellation deadline; job and audit assertions remain.

## Before tagging

1. Review and merge this preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.11`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
archives/checksums, `publication.json`, both image platforms, version/latest
digest identity and the distributed-image audit after publication. Local
qualification does not establish that those future publication steps passed.
