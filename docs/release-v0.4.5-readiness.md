# v0.4.5 Release Readiness

This record tracks the pre-tag validation for `v0.4.5`. The disposable-host
results are recorded separately in the release smoke result.

## Candidate

- Prepared: 2026-08-03 America/Toronto
- Release version: `v0.4.5`
- Release implementation commit: `32995a8`
- Release preparation branch: `codex/release-v0.4.5`
- Release tag: not created

## Scope

- Base Status recommendations on fresh, complete host facts and typed failure
  evidence.
- Reject SSH-backed maintenance before job creation when authentication or host
  trust is incomplete.
- Calculate the APT disk reserve from the exact simulated package plan, including
  split filesystems and a conservative fallback.
- Direct outdated sudo policies to the existing **Enable apt** repair path.
- Keep Scheduled Run reconciliation alive through transient manager and SQLite
  failures.
- Persist admitted notification deliveries in a redacted SQLite outbox.
- Keep Status command history fair by returning the eight newest commands per
  current server.
- Stabilize the periodic notification-outbox pruning test under the race detector.

## Post-Merge Main Gate

The implementation branch series was merged before release preparation. The
resulting `main` commit `32995a8` passed:

- CI run `30813303010`, including `ci-required`, Go tests, race tests, frontend
  unit tests, npm audit, and Playwright.
- CodeQL run `30813302473`, including Go, JavaScript/TypeScript, and Actions
  analysis.

## Local Automated Gate

The complete release gate passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...`
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (74.0% total)
- `go build -o webserver .`
- `npm ci`
- `npm run test:unit` (152 tests)
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:e2e` (62 tests)

Version metadata and repository-local Markdown links were checked across the
README, active documentation, release records, and Status version indicator.

## Docker Runtime Gate

The release candidate was validated on the configured remote Docker daemon:

- Docker Engine `29.6.2`, Linux `amd64`
- Candidate image:
  `sha256:1a36e67ba5bb44924efc65cb7ed8a17144899563c9b22e9b8057d5b66fbf19e9`
- Builder image:
  `golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2`
- Runtime image:
  `alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b`
- A fresh named volume initialized the SQLite database with UID/GID `100:101`
  and file mode `0600`.
- The `webserver` process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.

## Disposable-Host Gate

The live candidate passed the focused Status and update checks recorded in
[the v0.4.5 release smoke result](release-v0.4.5-smoke.md):

- Missing facts produced **Refresh host facts** before any maintenance guidance.
- Active APT work produced **Monitor APT**, pending updates produced **Review
  approval**, and the final state produced **Healthy**.
- The target SSH fingerprint was independently matched before trust was saved.
- The exact APT plan disk gate and explicit non-interactive strategy were present
  before mutation.
- Thirteen Ubuntu security updates installed successfully and the final scan
  completed with healthy APT state, no pending packages, and no reboot required.
- Status command history retained the complete seven-command sequence for the
  disposable target.

## Final Gate

- [x] All implementation PRs are merged to `main`.
- [x] Post-merge `main` CI and CodeQL are green on `32995a8`.
- [ ] The release-preparation PR checks are green.
- [ ] The release-preparation PR is merged to `main`.
- [ ] Post-merge `main` CI and CodeQL are green on the final release commit.
- [ ] The `v0.4.5` tag points to that verified final release commit.

No `v0.4.5` tag has been created by this preparation.
