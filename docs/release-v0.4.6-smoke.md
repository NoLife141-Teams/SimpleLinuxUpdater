# v0.4.6 Release Smoke Result

This result records the pre-tag Backup and live release-VM validation performed
for `v0.4.6`.

## Scope

- Date: 2026-08-04 America/Toronto
- Release implementation base: `0c385fe`
- Release preparation branch: `codex/release-v0.4.6`
- Disposable application target: `release-smoke-target`
- Release VM: Proxmox VM 117 (`ubuntutest`), `192.168.2.216`
- Target OS: Ubuntu 25.04
- Target kernel: `6.14.0-37-generic`

Credentials, encryption keys, private host data, target passwords, and session
cookies were kept out of the repository and this result.

## Snapshot And Admission

- The release owner confirmed that the VM's rollback snapshot was in place
  before this run.
- Independent Proxmox UI confirmation was unavailable because the existing web
  session required authentication; no snapshot mutation was attempted.
- The restored application credentials refreshed the target facts successfully,
  proving that the restored encrypted server secret was usable over SSH.

## Encrypted Backup Round-Trip

- A disposable candidate exported a `.slubkp` v1 archive with mode `0600`.
- Read-only verification reported a compatible, restore-ready archive containing
  one server and the known-hosts payload, with no blockers.
- A separate fresh application instance restored the archive successfully,
  invalidated prior sessions, and retained its own local encryption
  configuration while rewrapping restored secrets.
- The restored known-hosts SHA-256 matched the source exactly, the restored
  database contained the expected target, and a login with the archived
  disposable administrator succeeded.

## Live Host And APT

- Facts refresh reported 45.29 GiB free, healthy APT/DPKG state, and no reboot
  requirement.
- A normal scan passed disk, lock, and APT-health pre-checks, refreshed the
  Ubuntu repositories, and completed with zero pending packages.
- Because no package was pending, no upgrade or reboot was performed.
- A deliberately aggressive one-second timeout reproduced the timeout guard.
  The first run exposed a false ambiguous outcome after `apt update` had emitted
  progress and released its lock. The release branch now permits one bounded
  quiet-finalization window in that exact state.
- The corrected live rerun completed `apt update`; the following read-only
  package-list command then reached its intentionally strict one-second hard
  deadline. This is expected for non-mutating commands and does not trigger an
  unsafe APT replay.
- The new quiet-finalization regression passed 20 repeated focused Go runs with
  the existing timeout and lock-liveness suite.

## Intentionally Skipped

- Package installation and controlled reboot: the VM had no pending packages
  and did not require a reboot.
- Snapshot rollback: no host mutation occurred, so rollback was unnecessary.
- A live OSV coalescing burst: there were no pending package-version queries;
  concurrency and cancellation remain covered by the automated gate.

## Automated And Docker Gates

The complete final test counts, static checks, vulnerability checks, coverage,
and Docker runtime evidence are recorded in
[v0.4.6 Release Readiness](release-v0.4.6-readiness.md).
