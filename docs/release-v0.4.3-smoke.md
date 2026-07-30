# v0.4.3 Release Smoke Result

This result records the pre-tag validation performed for `v0.4.3`.

## Scope

- Date: 2026-07-30 America/Toronto
- Release implementation commit:
  `fa3af95747922438adfe4afa2bda82044b1d959d`
- Release preparation pull request: #349
- Release delta: bounded long-running APT command handling, legacy lock-probe
  compatibility, no-op notification suppression, and refreshed README demo

## Targeted APT and Notification Smoke

The targeted smoke used deterministic SSH sessions, command runners, and
notification delivery fixtures. It did not mutate packages on a real managed
host or deliver a notification to an external destination.

- Mutating APT commands received another timeout window only while the
  package-manager lock probe reported active work.
- APT commands timed out normally when no package-manager lock was active.
- Repeated active locks stopped extending the timeout after the configured
  three-extension cap.
- The timeout path retried the previous release's narrower lock-probe command
  when the extended probe was not permitted by the host's sudoers policy.
- Host pre-check lock inspection retained the same legacy sudoers
  compatibility.
- Non-APT commands were not eligible for lock-based timeout extension.
- Successful update-complete events with `upgrade_completed=false` did not
  enter notification delivery.

The following focused commands passed on the release implementation:

- `go test -count=1 -v . -run
  'TestRunSSHCommandWithTimeout(KeepsWaitingWhileAptLockIsActive|FallsBackToLegacyAptLockProbe|StillTimesOutAptWithoutActiveLock|CapsAptLockExtensions)'`
- `go test -count=1 -v ./internal/updates -run
  'TestIsAptLockProtectedCommand|TestProductionHostMaintenanceSessionFallsBackToLegacyLockProbe'`
- `go test -count=1 -v ./internal/notifications -run
  'TestNotificationDeliveryLifecycleSkipsNoOpUpdate'`

## Automated and Docker Gates

- PR #349 completed its required CI gate and all applicable CodeQL analyses.
- The post-merge `main` CI run completed frontend unit, npm audit, Go race,
  coverage, quality, Playwright, and required-gate jobs successfully on the
  release implementation commit.
- The post-merge CodeQL run completed successfully on the same commit.
- The full local and Docker candidate gates recorded in
  [v0.4.3 Release Readiness](release-v0.4.3-readiness.md) passed.
- The Docker candidate ran as non-root UID/GID `100:101`, initialized a fresh
  volume with the SQLite database at mode `0600`, returned HTTP `200`, and
  preserved state across restart.

## Intentionally Not Performed

No package installation was started on a real managed host solely for this
release smoke. That would create an unnecessary infrastructure side effect.
Timeout extension, lock-probe fallback, extension capping, and notification
suppression were instead exercised through controlled test doubles.
