# v0.4.3 Release Readiness

This record tracks the pre-tag validation for `v0.4.3`. It is intentionally a
readiness record, not a completed smoke result: the release tag must not be
created until every blocking item below is completed and the final commit SHA
is recorded.

## Candidate

- Prepared: 2026-07-30 America/Toronto
- Release version: `v0.4.3`
- Release source commit:
  `30dfbaafc41676b14eef79661402ed373c144f4b`
- Final release commit: pending
- Release tag: not created

## Scope

- Extend the timeout of a mutating APT command while a bounded liveness probe
  confirms that package-manager locks remain active.
- Preserve lock detection for hosts configured with the previous release's
  narrower passwordless sudoers rule.
- Avoid update-complete notifications when a successful run installs no
  upgrade.
- Refresh the README product demo.

## Local Automated Gate

The following checks passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (no reachable vulnerabilities)
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (72.6% total)
- `go build -o webserver .`
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:unit` (143 tests)
- `npm run test:e2e` (60 tests)

Version metadata was checked across `README.md`,
`docs/installation.md`, `docs/deployment.md`, `docs/configuration.md`,
`templates/index.html`, and this changelog.

## Docker Runtime Gate

The release candidate was validated on the configured remote Docker daemon:

- Docker Engine `29.6.2`, Linux `x86_64`
- `docker build --pull --tag simplelinuxupdater:v0.4.3-release-candidate .`:
  pass
- Candidate image:
  `sha256:e43c67f135468d5edd9bfe9c7ae2a753445e1bdb846c6b2e6211acf226a80080`
- Builder image:
  `golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2`
- Runtime image:
  `alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b`
- A fresh named volume initialized the SQLite database with UID/GID `100:101`
  and file mode `0600`.
- The `webserver` process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.
- The temporary container was stopped gracefully and removed.
- The temporary volume `slu-v043-rc-data`, which contains only the fresh smoke
  database, is intentionally left for manual cleanup and does not block the
  release.

## Targeted Smoke

The following focused checks passed:

- Bounded APT timeout extensions continue only while the package-manager lock
  probe reports active work.
- APT commands still time out when no package-manager lock is active and stop
  extending after the configured cap.
- Both timeout and host-inspection paths fall back to the legacy lock probe
  when the extended sudoers command is unavailable.
- Successful no-op updates do not enter the notification delivery lifecycle.

These behaviors were exercised with:

- `go test -count=1 -v . -run
  'TestRunSSHCommandWithTimeout(KeepsWaitingWhileAptLockIsActive|FallsBackToLegacyAptLockProbe|StillTimesOutAptWithoutActiveLock|CapsAptLockExtensions)'`
- `go test -count=1 -v ./internal/updates -run
  'TestIsAptLockProtectedCommand|TestProductionHostMaintenanceSessionFallsBackToLegacyLockProbe'`
- `go test -count=1 -v ./internal/notifications -run
  'TestNotificationDeliveryLifecycleSkipsNoOpUpdate'`

## Blocking Before Tag

- [ ] Record the final release commit SHA above.
- [ ] Confirm CI jobs are green on that exact commit.
- [x] Complete targeted smoke coverage for bounded APT timeout extension,
      legacy sudoers compatibility, and no-op notification suppression.

No `v0.4.3` tag has been created by this preparation.
