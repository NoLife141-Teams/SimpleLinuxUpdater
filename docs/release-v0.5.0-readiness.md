# v0.5.0 Release Readiness

Pre-tag preparation. This branch creates no tag, GitHub Release or registry image.

## Candidate and scope

- Prepared: 2026-10-29 America/Toronto.
- Previous published version: `v0.4.16` (`60432cac60d08722cdf0034a000d059a81ee967d`).
- Implementation base: `6da93c7cd16e09a0e8049d71c61735a09548df8f` on `main`.
- Preparation branch: `codex/prepare-v0.5.0`.
- README, displayed version, installation/deployment/configuration examples and
  changelog are aligned to `v0.5.0`.
- Preparation changes version metadata, documentation and the qualification
  harness SSH log level. Application Go code,
  database migrations, SSH helper and dependencies retain the implementation
  base's behavior. SQLite remains at `v1.60.1`; PR #510 is not part of this release.
- Builds and archives use a clean source checkout. Local untracked prototypes
  and existing installation data are excluded.

## Upgrade requirements

Follow [Upgrading from v0.4.16 to v0.5.0](upgrading.md) and the
[v0.5.0 compatibility notes](../CHANGELOG.md#upgrade-notes).

Export and verify an encrypted pre-upgrade backup, pause scheduled work and stop
old processes before replacing the executable and assets together. Account and
policy migrations are additive. The original account ID, username and password
hash survive; the account becomes administrator and existing browser sessions
are revoked once. New sessions survive subsequent restarts. Never run an older
executable on the migrated database; rollback restores the pre-upgrade archive
into a separate installation of the old version.

Package Deferrals require the current protocol 3 managed root helper, including
root SSH with active deferrals. Missing/old helpers block protected maintenance.
Review and install the exported helper on each intended target. Existing daily
and weekly policies retain automatic canaries. Queued/running approvals cannot
resume after migration: reconcile interrupted package mutations, rescan and
obtain fresh approval.

Automatic backups are opt-in and require the documented paired destination and
private passphrase-file settings. Restored backup automation remains suspended
until local revalidation. Restore invalidates browser sessions/personal API
tokens and archived recovery codes; follow the MFA and separate metrics-token
restore rules. New vulnerability scanning settings and defaults are documented
in [Configuration](configuration.md) and [Usage](usage.md).

## Qualification

Validation is in progress. Results below will be replaced with measured evidence
before the preparation PR is marked ready. Prior release records are historical
and do not establish qualification of this candidate.

## Before tagging

1. Review and merge this preparation PR only after its CI and CodeQL succeed.
2. Verify CI and CodeQL on the exact merged `main` commit.
3. Verify the active `Protect release tags v*` ruleset and resolve any recorded
   qualification failures or limits below.
4. Obtain separate explicit authorization to tag and publish `v0.5.0`.

Publication must use the existing tag-signal/trusted-release workflow. That
workflow must build and qualify its own archives and both image architectures,
scan OS and Go packages, and validate provenance before promoting a digest or
publishing a release. Local preparation artifacts are not publication bytes.
