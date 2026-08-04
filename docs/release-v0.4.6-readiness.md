# v0.4.6 Release Readiness

This record tracks the pre-tag validation for `v0.4.6`. The live VM and Backup
results are recorded separately in the release smoke result.

## Candidate

- Prepared: 2026-08-04 America/Toronto
- Release version: `v0.4.6`
- Release implementation base: `0c385fe`
- Release preparation branch: `codex/release-v0.4.6`
- Release tag: not created

## Scope

- Keep canary rollout progress scoped to the exact policy occurrence.
- Select current Status jobs correctly and reject stale secondary responses.
- Reject ambiguous Backup archives and reduce export, restore, and encryption
  allocation growth without changing the `.slubkp` v1 format.
- Coalesce identical concurrent OSV queries.
- Cancel abandoned Dashboard projections and compact the initial Status payload.
- Treat lock-protected APT timeouts as inactivity windows, including one bounded
  quiet-finalization window after observed progress.
- Keep all local `.tmp*` smoke workspaces out of Docker build contexts.

## Post-Merge Main Gate

The implementation PR series was merged before release preparation. The
resulting `main` commit `0c385fe` passed:

- CI run `30956676565`, including the required Go, race, frontend, audit, and
  Playwright checks.
- CodeQL run `30956676292`, including Go, JavaScript/TypeScript, and Actions
  analysis.

## Local Automated Gate

The complete release gate passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (zero reachable vulnerabilities)
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (73.4% total;
  root package 74.4%; updates package 81.0%)
- `go build -o webserver .`
- `npm run test:unit` (157 tests)
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:e2e` (63 tests)

Version metadata is consistently `v0.4.6`, `git diff --check` passed, and 191
repository-local Markdown links were resolved successfully across active docs.

## Docker Runtime Gate

The release candidate passed the runtime gate on the configured remote daemon:

- Docker Engine `29.6.2`, Linux `amd64`
- Candidate image:
  `sha256:b736d51cf831c9044979c3783a6213c6b32d9903da16b6e7ffbe1c6d688d5ca2`
- Builder image: `golang:1.26.5-alpine`
- Runtime image: `alpine:3.24`
- The final build context was 18.60 kB and excluded local `.tmp*` smoke data.
- A fresh named volume initialized SQLite with UID/GID `100:101` and mode
  `0600`; the application process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.

## Live VM And Backup Gate

The focused live checks are recorded in
[the v0.4.6 release smoke result](release-v0.4.6-smoke.md):

- Encrypted Backup export, read-only verification, and restore round-trip.
- Restored SSH credential usability against the release VM.
- Host facts, APT health, normal package discovery, and the aggressive timeout
  guard on the live VM.

## Final Gate

- [x] All implementation PRs are merged to `main`.
- [x] Post-merge `main` CI and CodeQL are green on `0c385fe`.
- [x] The complete local and Docker release gate is green.
- [ ] The release-preparation PR is green and merged to `main`.
- [ ] Post-merge `main` CI and CodeQL are green on the final release commit.
- [ ] The `v0.4.6` tag points to that verified final release commit.

No `v0.4.6` tag has been created by this preparation.
