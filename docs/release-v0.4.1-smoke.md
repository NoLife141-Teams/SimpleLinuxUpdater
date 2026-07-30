# v0.4.1 Release Smoke Result

This result records the pre-tag validation performed for `v0.4.1`.
Credentials, private keys, session cookies, backup passphrases, and backup
archives are intentionally omitted.

## Scope

- Date: 2026-07-30 UTC
- Release implementation commit:
  `bb05c8ab29a0bbbf3aadb24bfc7e6b9db579fec4`
- Pull request: #343
- Disposable Docker network: `slu-v041-smoke`
- Disposable application container: `slu-v041-smoke-app`
- Disposable application data volume: `slu-v041-smoke-app-data`
- Disposable target: `release-smoke-target`
- Target container image: `ubuntu:24.04`
- Target OS: Ubuntu 24.04.4 LTS
- Target architecture: Linux `x86_64`

## Setup and Authentication

- First-run setup created the disposable local administrator and redirected to
  the Status workspace.
- Logout redirected to `/login`.
- An invalid password was rejected with the visible `invalid credentials`
  alert.
- The valid password restored the authenticated Status workspace.
- The Status workspace displayed version `v0.4.1`.

## Host Registration and Trust

- An empty server payload was rejected with HTTP `400` and the required-field
  explanation.
- `release-smoke-target` was registered with an explicit per-server Ed25519
  private key and the `release-smoke` tag.
- Non-destructive SSH login, `sudo -n`, OS inspection, and
  `apt-get -s upgrade` passed before application actions.
- The application scanned the target's ECDSA host key. Its
  `SHA256:xrKbHXjEO0CTLx2TOqtFwK6k4EpehAWtOO/QAmy560c` fingerprint matched the
  key read directly from the disposable target.
- Trust persisted exactly one entry in the disposable application
  `known_hosts` file.
- Host-fact refresh reported healthy APT and disk state, no reboot requirement,
  and Ubuntu 24.04.4 LTS.

## Update Flow

- A deliberately incomplete first target image lacked `/usr/bin/fuser`.
  The updater correctly failed the `apt_locks` precheck before running
  `apt-get update`. Installing the target prerequisite package `psmisc`
  resolved the harness issue.
- A concurrent duplicate update request returned HTTP `409`.
- Package discovery transitioned to `pending_approval` with 13 upgradeable
  packages.
- Full-upgrade approval was accepted and all 13 packages were installed.
- The minimal container does not run systemd. The first post-update check
  therefore reported the expected missing-`systemctl` harness limitation after
  the successful package upgrade.
- A disposable no-failed-units `systemctl` harness command was then installed.
  A repeat update scan completed with status `done`, zero pending packages,
  healthy APT, healthy disk, and no reboot requirement.
- Dashboard Projection reported one done host, zero in-progress hosts, zero
  pending approvals, and zero pending packages.

## Scheduled Policy

- A disabled daily `scan_only` policy targeting only
  `release-smoke-target` created no run.
- The policy list resolved exactly that one matched server.
- Enabling the policy for the next UTC minute created one scheduled run.
- The run completed with status `succeeded` and summary
  `Scheduled scan completed: no pending updates`.
- The run exposed a working Markdown job-report URL.

## Reports and Observability

- Filtered activity history contained host registration, key upload, facts,
  update, approval, scheduled-run, and completion events.
- Audit and scheduled-job Markdown reports returned HTTP `200`.
- Observability summaries returned HTTP `200` for `24h`, `7d`, and `30d`.
- The 7-day summary reported three update attempts: one successful final scan
  and two documented harness-related failures.
- Host-health trends contained six observations, including the healthy facts
  snapshot and the successful scheduled scan.

## Backup

- Encrypted backup export returned HTTP `200` with a non-empty `.slubkp`
  archive and a completed backup job identifier.
- Non-mutating verification returned HTTP `200`.
- The archive was valid, compatible, restore-ready, and contained
  `servers.db`, `config.json`, and the disposable `known_hosts`.
- Database and configuration validation passed with no blockers.

## Timeout Guard

- The application was restarted against the same disposable volume with
  `DEBIAN_UPDATER_SSH_COMMAND_TIMEOUT_SECONDS=1`.
- A disposable target-side `apt-get autoremove` harness delay exceeded the
  timeout.
- The command retried three times, ended in explicit `error`, and recorded
  `command timed out after 1s`, retry exhaustion, and attempt counts.
- No host remained in an active maintenance state.

## Automated and Docker Gates

- The full local gate recorded in
  [v0.4.1 Release Readiness](release-v0.4.1-readiness.md) passed.
- PR #343 CI, required gate, npm audit, frontend tests, race, coverage,
  quality, and CodeQL checks passed.
- The Docker candidate built from the merged implementation tree, ran as
  non-root UID `100`, initialized the disposable volume, served HTTP `200`,
  and preserved state across restart.

## Intentionally Skipped

- Backup restore was not executed because export plus non-mutating verification
  covered archive readiness without replacing working state.
- Sudoers enable/disable, credential removal, audit pruning, metrics-token
  rotation, and external notification delivery were not exercised because
  their side effects were unrelated to the release delta and their automated
  coverage passed.
