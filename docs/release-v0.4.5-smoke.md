# v0.4.5 Release Smoke Result

This result records the pre-tag disposable-host validation performed for
`v0.4.5`.

## Scope

- Date: 2026-08-03 America/Toronto
- Release implementation commit: `32995a8`
- Release preparation branch: `codex/release-v0.4.5`
- Candidate image:
  `sha256:1a36e67ba5bb44924efc65cb7ed8a17144899563c9b22e9b8057d5b66fbf19e9`
- Disposable app container: `slu-v045-rc`
- Disposable target: `release-smoke-target`
- Target OS: Ubuntu 24.04.4 LTS container

Credentials, private keys, temporary target passwords, and session cookies were
kept out of the repository and this result.

## Status Guidance And SSH Admission

- A newly registered target with missing facts displayed **Refresh host facts**
  in both the maintenance table and selected-host inspector.
- Refreshing facts changed the recommendation to **Healthy** and populated OS,
  kernel, disk, APT, reboot, and freshness facts.
- The application proposed the target's ECDSA host key, and its SHA-256
  fingerprint matched an independent console reading before trust was accepted.
- The target was admitted with explicit per-host authentication and trusted-host
  posture; no maintenance job was created before those requirements were met.
- During discovery and upgrade, Status displayed **Monitor APT**. At approval it
  displayed **Review approval**, and after the successful verification run it
  displayed **Healthy**.

## Package Plan And Upgrade

- Baseline disk, APT lock, and APT/DPKG health checks passed.
- The plan-aware gate reported an exact APT plan of 0.01 GiB of archives, 0.00
  GiB installed growth, 0.06 GiB safety, and a 1.00 GiB reserve against 886.17
  GiB free.
- Before mutation, the log recorded the non-interactive frontend, disabled
  changelogs, automatic `needrestart` handling, and keep-existing-conffile
  strategy.
- Discovery found 13 standard security updates and no kept-back packages. The
  security-only approval installed all 13 packages.
- The minimal target image initially lacked `systemctl`, so its first failed-unit
  post-check correctly produced **Review failure** even though APT succeeded. A
  deterministic no-failed-units fixture was then supplied for this container-only
  check.
- The next application scan completed successfully. Final Status showed zero
  pending packages, healthy APT, fresh facts, no reboot requirement, and
  **Healthy**.

## Persistence And History

- The candidate initialized a fresh SQLite volume with restrictive ownership and
  mode, survived an application-container restart, and continued serving the
  setup endpoint.
- The selected-host history retained all seven commands from registration through
  the failed environmental post-check and successful verification scan, while the
  dashboard returned no unrelated host history.
- Notification outbox restart recovery, redaction, terminal retention, and retry
  behavior passed the complete automated Go and race gates. No external
  notification receiver was configured for this disposable run.
- Scheduled-run reconciliation through transient manager and SQLite failures
  passed the automated gate. A live failure was not injected because reproducing
  it requires deliberately destabilizing the candidate database or runtime.

## Intentionally Skipped

- Controlled reboot: the target was not a real `systemd` boot environment and
  cannot prove uptime reset or SSH recovery.
- Notification delivery: no disposable external receiver was configured.
- Backup restore: the v0.4.5 delta does not change backup behavior; the existing
  encrypted export and read-only verification flow remains covered by automated
  tests and the v0.4.4 live smoke.
- Additional rollout waves: the disposable fleet contained only one target, and
  the v0.4.5 scheduling change concerns reconciliation rather than rollout order.

## Automated And Docker Gates

The full local gate passed with 152 frontend unit tests, 62 Playwright tests,
74.0% Go coverage, zero npm vulnerabilities, no reachable Go vulnerability, and
successful vet, static analysis, race, build, and Actions-workflow checks. The
Docker candidate also passed non-root execution, database ownership and mode,
fresh setup, and restart persistence checks. Full command, CI, and image details
are recorded in
[v0.4.5 Release Readiness](release-v0.4.5-readiness.md).
