# v0.4.7 Release Smoke Result

This result records the pre-tag fresh-database and disposable-host validation
performed for `v0.4.7`.

## Scope

- Date: 2026-08-05 America/Toronto
- Release implementation base: `0055d97`
- Release preparation branch: `codex/release-v0.4.7`
- Candidate image:
  `sha256:6c8e1b8ae30b6232c1f412cb715ed4ec4177eff4e08e60387e7fdfc181e2d406`
- Disposable application container: `slu-v047-app`
- Disposable application database: fresh SQLite volume
- Disposable application target: `release-smoke-target`
- Target OS: Debian GNU/Linux 12 (bookworm) container

Administrator credentials, private keys, session cookies, and private host data
were generated only for this isolated run and kept out of the repository.

## Fresh Setup And SSH Admission

- The candidate opened `/setup`, created a new administrator, and displayed the
  `v0.4.7` Status dashboard without using an existing application account.
- A disposable SSH key was generated for this run and uploaded as the target's
  per-server credential.
- The application scanned the target's ECDSA host key, displayed its SHA-256
  fingerprint, and saved trust only after explicit confirmation.
- Manage Servers then showed one target with **Per-server key** and **Host key
  trusted**, with no missing-authentication or host-trust warning.

## Status Guidance And APT Scan

- Before collection, the maintenance table and selected-host inspector both
  displayed **Refresh host facts** instead of a generic update action.
- Facts refresh populated Debian version, kernel, disk, APT, reboot, and
  freshness details and changed the recommendation to **Healthy**.
- The first scan intentionally exercised the typed pre-check failure path because
  the minimal Debian image lacked `psmisc` and therefore `/usr/bin/fuser`.
- After installing the documented prerequisite, Docker's restricted `/proc`
  view still prevented lock inspection. The same preserved target was restarted
  with the container capabilities required to model a normal server process
  view; the SSH identity and application trust record remained unchanged.
- The representative rerun logged the explicit non-interactive APT strategy,
  passed disk-space, lock-contention, and APT/DPKG health pre-checks, refreshed
  all Debian repositories, and found no packages to upgrade.
- The final Status state was **Done** and **Healthy**, with zero pending, kept
  back, security, or CVE-counted packages; APT was healthy, facts were fresh,
  and no reboot was required.
- The missing `systemctl` baseline warning is expected for this container target
  and did not prevent the successful package scan.

## Persistence And Operator Surfaces

- Restarting the application container against the same fresh SQLite volume kept
  the authenticated session, registered target, SSH trust, facts, job result,
  and **Healthy** recommendation.
- Observability loaded the target health trend and represented the two typed
  `precheck:apt_locks` environmental failures followed by the successful run.
- Admin loaded account-security, policy, scheduled-run, Backup, notification,
  and Metrics sections against the fresh database.
- The disposable containers, SSH network, and local private key files were
  removed after evidence collection.

## Intentionally Skipped

- Package approval and installation: the fresh Debian target had no pending
  packages after repository refresh.
- Controlled reboot: the target was not a real `systemd` boot environment and
  cannot prove uptime reset or SSH recovery.
- Encrypted Backup restore: the v0.4.7 delta does not change Backup behavior;
  archive validation and restore remain covered by the automated gate and the
  v0.4.6 live smoke.
- External notifications: no disposable third-party receiver was configured.
- Multi-host rollout waves: the v0.4.7 delta is limited to the existing Status
  experience and the disposable fleet contained one target.

## Automated And Docker Gates

The complete test counts, static checks, vulnerability checks, coverage, Docker
runtime evidence, and final release conditions are recorded in
[v0.4.7 Release Readiness](release-v0.4.7-readiness.md).
