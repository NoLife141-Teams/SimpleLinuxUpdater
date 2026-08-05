# v0.4.7 Release Readiness

This record tracks the pre-tag validation for `v0.4.7`. The disposable-host
results are recorded separately in the release smoke result.

## Candidate

- Prepared: 2026-08-05 America/Toronto
- Release version: `v0.4.7`
- Release implementation base: `0055d97`
- Release preparation branch: `codex/release-v0.4.7`
- Release tag: not created

## Scope

- Prioritize the Status maintenance timeline by recommended action while
  preserving explicit name and status sorting.
- Align stale and unknown host-fact counters, filters, and accessibility labels.
- Reduce duplicate Status surfaces and keep supporting details available on
  demand.
- Clarify immediate bulk-action eligibility and retain executed, skipped, and
  failed results.
- Keep the selected-host inspector accessible on desktop and mobile.
- Keep degraded Status synchronization visible with source and freshness
  details.
- Stabilize the notification wake retry test under loaded CI runners.
- Update Playwright from 1.62.0 to 1.62.1.

## Post-Merge Main Gate

The implementation PR series was merged before release preparation. The
resulting `main` commit `0055d97` passed:

- CI run `31008737826`, including the required Go, race, frontend, audit, and
  Playwright checks.
- CodeQL run `31008737175`, including Go, JavaScript/TypeScript, and Actions
  analysis.

## Local Automated Gate

The complete release gate passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (zero reachable vulnerabilities)
- `actionlint`
- `go test -count=1 ./...`
- focused route-contract and package tests
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (73.4% total;
  root package 74.4%; notifications package 79.2%; updates package 81.0%)
- `go build -o webserver .`
- `npm ci`
- `npm run test:unit` (168 tests)
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:e2e` (67 tests)

Version metadata is consistently `v0.4.7`. Repository-local Markdown links and
the final worktree diff are checked again before the release commit.

## Docker Runtime Gate

The release candidate passed the runtime gate on the configured remote daemon:

- Docker Engine `29.6.2`, Linux `amd64`
- Candidate image:
  `sha256:6c8e1b8ae30b6232c1f412cb715ed4ec4177eff4e08e60387e7fdfc181e2d406`
- Builder image: `golang:1.26.5-alpine`
- Runtime image: `alpine:3.24`
- The final build context was 137.92 kB.
- A fresh named volume initialized SQLite with UID/GID `100:101` and mode
  `0600`; the application process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.

## Disposable-Host Gate

The focused live checks are recorded in
[the v0.4.7 release smoke result](release-v0.4.7-smoke.md):

- A fresh database completed first-run setup without relying on an existing
  administrator account.
- A disposable Debian SSH target was admitted with an independently generated
  per-host key and explicit host-key trust.
- Missing facts produced **Refresh host facts** in the table and inspector;
  collected facts then produced **Healthy**.
- The explicit non-interactive APT strategy and disk, lock, and health pre-checks
  appeared before repository refresh.
- The final scan completed successfully with zero pending packages, fresh facts,
  healthy APT state, and no reboot requirement.
- Status state, history, authentication, and the successful run survived an
  application-container restart.
- Observability reflected the typed environmental failures and final success;
  Admin loaded against the same fresh database.

## Final Gate

- [x] All implementation PRs are merged to `main`.
- [x] Post-merge `main` CI and CodeQL are green on `0055d97`.
- [x] The complete local, Docker, and disposable-host release gate is green.
- [ ] The release-preparation PR is green and merged to `main`.
- [ ] Post-merge `main` CI and CodeQL are green on the final release commit.
- [ ] The `v0.4.7` tag points to that verified final release commit.

No `v0.4.7` tag has been created by this preparation.
