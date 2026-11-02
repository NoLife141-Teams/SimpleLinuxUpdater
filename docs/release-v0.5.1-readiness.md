# v0.5.1 release readiness

Preparation only: no merge, tag, GitHub Release, registry push or deployment.

## Candidate and scope

- Prepared: 2026-11-02, America/Toronto.
- Published baseline: `v0.5.0`.
- Implementation base: `d1ba7722ccb06561334e4792b5c0d5477314e016` (PR #540).
- Preparation branch: `codex/stabilize-cve-v0.5.1`; tracking issue #541.
- Includes the nine CVE PRs #532–#540 already merged after v0.5.0, focused
  workflow fixes and version/documentation updates. SQLite PR #510 stays separate.
- Go remains pinned to 1.26.10; no dependency or release-workflow change.

## Upgrade and rollback

Follow [the upgrade guide](upgrading.md). v0.5.0-to-v0.5.1 adds remediation
history/risk-review tables and richer JSON evidence. Old observations remain
readable without being upgraded into current proof. No new required environment
variable or SSH helper protocol. Export and verify an encrypted backup, finish
or cancel maintenance and replace the executable and assets together. Restore
that backup into a separate old-version installation for rollback. Earlier
v0.4.16 upgrades retain the v0.5.0 account, policy and helper requirements.

## Qualification

Validation is in progress on the preparation branch. Final measured results and
CI links will replace this paragraph before the PR is declared ready.

## Scope of evidence

Browser scenarios use the production router, authentication, SQLite persistence,
maintenance runner and independent verification lifecycle in disposable test
installations. Host Maintenance Session and CVE provider transports use synthetic
fixtures: no managed server or public CVE service is contacted. Desktop and
mobile runs establish workflow behavior, not real-world fix availability or
runtime exploitability. Existing package-evidence tests cover partial scans,
source/provenance drift, cancelled work and independent health outcomes.

Docker is unavailable on this macOS workstation. Linux CI must qualify full Go
race/coverage, Docker amd64/arm64, the pinned published-v0.4.16 upgrade and
Debian 13/Ubuntu 24.04 SSH/APT fixtures. Those fixtures do not replace a live
external-provider or production deployment check. Do not relabel the v0.5.0
release evidence or an earlier extended-matrix run as qualification of this head.

## Before publication

Review this PR and its final CI/CodeQL results. Merge and publication require
separate authorization; after merge, recheck the exact main commit, tag ruleset
and all required release gates. The trusted release workflow must build and
qualify its own exact tagged archives and images before publication.
