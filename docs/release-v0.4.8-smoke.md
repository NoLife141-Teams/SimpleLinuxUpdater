# v0.4.8 Release Smoke Result

This record will contain the pre-tag Docker and disposable-host evidence for
`v0.4.8`. Unchecked items are not claimed as passed.

## Scope

- Date: 2026-09-03 America/Toronto
- Release implementation base: `b8f36e1`
- Release preparation branch: `release/v0.4.8`
- Candidate image: `simplelinuxupdater:v0.4.8-rc`
- Candidate image ID:
  `sha256:d28cefdf7695595a886cedbecd1d71fd8a95138d22cd02e5d954dc4d17e14be8`
- Docker daemon: Docker Desktop 29.7.2, Linux amd64, reached through the
  `windows-pc` context after the host firewall rule was corrected
- Disposable application database: fresh persistent storage
- Disposable SSH targets: Debian 12 non-root and root accounts on an isolated
  Docker network

All credentials, keys, cookies, and target data must remain disposable and out
of the repository.

## Docker and Application Lifecycle

- [x] Candidate image builds from the branch with Go 1.26.6 and Alpine 3.24.
- [x] Container runs as UID 100/GID 101 rather than root.
- [x] Explicit image listener `:8080` is reachable on the isolated smoke
  network; image metadata and logs confirm the Docker-specific override.
- [x] `/data` accepts SQLite/config creation as UID 100/GID 101; `/data` is mode
  0700 and `servers.db` is mode 0600.
- [x] Fresh `/setup` creates the administrator and `/login` authenticates it.
- [x] Restarting the same candidate with the same storage preserves login and
  application state.

## SSH, Facts, and Package Maintenance

- [x] Admit a disposable non-root target with explicit host-key scan and trust.
- [x] Refresh facts live for non-root and root accounts; the scheduled refresh
  integration remains covered by the automated gate.
- [x] Install the typed root helper and owner-marked sudoers policy with
  **Enable apt**.
- [x] Verify APT metadata refresh, package discovery, upgrade, full-upgrade,
  lock probes, repair, exact selected-package dispatch, and autoremove. The
  target was current, so upgrade mutations completed as safe no-ops; uncertain
  outcome reconciliation remains covered by deterministic automated tests.
- [x] Verify equivalent APT update and discovery through a root SSH account.

## Sudoers Boundary and Migration

- [x] Confirm no active managed rule grants arbitrary `apt` or `apt-get`, and
  prove `sudo -n /usr/bin/apt-get update` remains denied.
- [x] Confirm raw options, hooks, shell metacharacters, paths, and invalid package
  selectors are refused by the typed helper.
- [x] Migrate an exact legacy app-generated `/etc/sudoers.d/apt-nopasswd` rule.
- [x] Confirm an unrecognized legacy file is preserved byte-for-byte and blocks
  automatic enable/disable with actionable guidance.
- [x] Confirm disable removes the recognized owner-marked helper and sudoers
  file, with no legacy file present or removed unexpectedly.

## Intentionally Skipped

- Controlled reboot was not invoked. A Docker container without a real init
  system cannot prove host reboot behavior safely; the typed reboot allowlist,
  confirmation route, root/non-root command selection, and outcome handling are
  covered by the automated gate.
- The first Debian target exposed Docker Desktop's restricted `/proc/*/fd`
  behavior to `fuser`; the application correctly failed closed before APT. A
  second disposable target with only `SYS_PTRACE` added produced clean no-lock
  semantics and passed the complete maintenance smoke. This capability is a
  smoke-harness accommodation, not an application deployment requirement.
- No release tag, GitHub Release, or image publication was performed.
- The automated gate covers setup/login, SSH adapters, host-facts refresh,
  typed APT effects and reconciliation, root-helper input rejection, sudoers
  path ownership/content guards, root SSH command selection, persistence, and
  frontend/CSP contracts. These tests do not replace the unchecked live smoke
  items above.

## Automated Gate

Test counts, coverage, static analysis, vulnerability results, workflow
validation, and final release conditions are recorded in
[v0.4.8 Release Readiness](release-v0.4.8-readiness.md).
