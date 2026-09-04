# v0.4.8 Release Smoke Result

This record will contain the pre-tag Docker and disposable-host evidence for
`v0.4.8`. Unchecked items are not claimed as passed.

## Scope

- Date: 2026-09-03 America/Toronto
- Release implementation base: `b8f36e1`
- Release preparation branch: `release/v0.4.8`
- Candidate image: not built; no configured Docker daemon was reachable
- Disposable application database: fresh persistent storage
- Disposable SSH targets: non-root and root Debian-compatible targets

All credentials, keys, cookies, and target data must remain disposable and out
of the repository.

## Docker and Application Lifecycle

- [ ] Candidate image builds from the branch.
- [ ] Container runs as a non-root final process.
- [ ] Explicit image listener `:8080` is reachable through the published port.
- [ ] `/data` accepts SQLite/config creation with safe ownership and permissions.
- [ ] Fresh `/setup` creates the administrator and `/login` authenticates it.
- [ ] Restarting the same candidate with the same storage preserves login and
  application state.

## SSH, Facts, and Package Maintenance

- [ ] Admit a disposable non-root target with explicit host-key trust.
- [ ] Refresh host facts and verify the periodic/scheduled refresh integration.
- [ ] Install the typed root helper and owner-marked sudoers policy with
  **Enable apt**.
- [ ] Verify APT metadata refresh, package discovery, mutation/reconciliation,
  lock probes, repair, package selection, autoremove, and controlled reboot as
  safely applicable to the target.
- [ ] Verify equivalent direct maintenance through a root SSH account.

## Sudoers Boundary and Migration

- [ ] Confirm no active managed rule grants arbitrary `apt` or `apt-get`.
- [ ] Confirm raw options, hooks, shell metacharacters, paths, and invalid package
  selectors are refused by the typed helper.
- [ ] Migrate an exact legacy app-generated `/etc/sudoers.d/apt-nopasswd` rule.
- [ ] Confirm an unrecognized legacy file is preserved byte-for-byte and blocks
  automatic enable/disable with actionable guidance.

## Intentionally Skipped

- Docker/application and disposable-host smoke remain pending. Both available
  remote daemons timed out over SSH and the local default Docker socket is not
  present. No Docker context was changed and no release image was published.
- The automated gate covers setup/login, SSH adapters, host-facts refresh,
  typed APT effects and reconciliation, root-helper input rejection, sudoers
  path ownership/content guards, root SSH command selection, persistence, and
  frontend/CSP contracts. These tests do not replace the unchecked live smoke
  items above.

## Automated Gate

Test counts, coverage, static analysis, vulnerability results, workflow
validation, and final release conditions are recorded in
[v0.4.8 Release Readiness](release-v0.4.8-readiness.md).
