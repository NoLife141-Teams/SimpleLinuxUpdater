# v0.4.10 Release Smoke Result

## Scope

- Date: 2026-09-07 America/Toronto.
- Implementation: `973a715495f6688b4bb81112a81399dbfcbfe1a9`, with v0.4.10
  metadata on `codex/prepare-v0.4.10`.
- Application: native macOS arm64 build using Go 1.26.6, disposable source and
  restore databases, and separate disposable `known_hosts` files.
- SSH target: release-owned Debian 12 arm64 container, non-root `smoke` account,
  loopback-only published SSH port. No saved host or production data was used.
- Browser coverage: 68 automated Playwright tests. Live SSH and restore checks
  used an HTTP harness against the real application, not a manual Computer Use
  pass or mocked server responses.

## Passed

- Fresh account setup, host inventory creation, host-key scan and trust after
  matching the fingerprint to the disposable target's own host-key file.
- **Enable apt** installed the typed helper and sudoers policy for a non-root
  SSH account; live host-fact refresh completed.
- Real package discovery and baseline APT/disk/lock checks reached pending
  approval with a persisted job ID and nonzero approval generation.
- An incorrect approval generation and duplicate update were both rejected
  with HTTP 409. Approval with the displayed identity was accepted.
- The real update upgraded `libpcre2-8-0` from `10.42-1` to
  `10.42-1+deb12u1`, retained progress output, passed the plan-aware disk check
  and post-update APT health check, and reached a successful persisted job.
- A disabled scan-only policy was created, previewed with an explicit target,
  configured for one-host canary/waves, enabled for the next local minute, and
  shown in the calendar. Its real scheduled scan succeeded. It was disabled
  again before backup.
- Audit, job Markdown report, Observability summary and host-health-trend APIs
  returned successful responses after the update and scheduled scan.
- Encrypted backup export included `known_hosts`; verification succeeded and
  wrong-passphrase verification failed without restoring.
- Restore into a separate disposable runtime succeeded, invalidated its prior
  session, accepted the restored account, preserved the expected inventory,
  and released maintenance.
- An official, checksum-verified v0.4.9 macOS arm64 archive initialized a separate
  database. Starting the v0.4.10 build on that database preserved the existing
  account login and encrypted server inventory without requiring setup again.
- Both container architectures passed startup, non-root PID 1, explicit non-root
  restart and account persistence; both OS/Go scans had zero HIGH/CRITICAL
  findings. All five archive checksums and formats passed, and the packaged
  Linux amd64 binary served `/login` under Linux.

## Environment limitations and corrections

The first HTTP harness revision read job status outside the response's `job`
object; it was corrected and rerun with fresh data. The initial SSH container
lacked `SYS_PTRACE`, so `fuser` could not inspect other users' file descriptors;
the application correctly stopped at its lock pre-check. The disposable target
was recreated with that capability, without changing the application's checks.

The target has no systemd PID 1. With the default configuration, the package
upgrade completed but its failed-unit post-check correctly reported an error.
The successful container pass used
`DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_FAILED_UNITS=false` only in the disposable app
environment. The unavailable systemd check remained a warning; APT health
checks remained blocking. This does not qualify real systemd service health.

Controlled reboot was not run because this is not a real systemd host.
Multi-host downstream waves and external notification delivery were not run:
there was one disposable target and no external receiver. Their automated
regressions passed. Unknown-outcome timeout, stale publication, and failed
restore recovery are covered by deterministic regression tests, not induced
against the live target. No production deployment, tag or publication occurred.

Temporary credentials, cookies, databases and raw smoke artifacts remain outside
tracked files. The disposable target was stopped and removed after validation.
