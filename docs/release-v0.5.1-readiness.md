# v0.5.1 release readiness

Preparation only: no merge, tag, GitHub Release, registry push or deployment.

## Candidate and scope

- Prepared: 2026-11-02, America/Toronto.
- Published baseline: `v0.5.0`.
- Implementation base: `d1ba7722ccb06561334e4792b5c0d5477314e016` (PR #540).
- Preparation branch: `codex/stabilize-cve-v0.5.1`; tracking issue #541.
- Includes the nine CVE PRs #532–#540 already merged after v0.5.0, focused
  workflow fixes and version/documentation updates. SQLite PR #510 stays separate.
- Go remains pinned to 1.26.10; no dependency or release-workflow change.

## Upgrade and rollback

Follow [the upgrade guide](upgrading.md). v0.5.0-to-v0.5.1 adds remediation
history/risk-review tables and richer JSON evidence. Old observations remain
readable without being upgraded into current proof. No new required environment
variable or SSH helper protocol. Export and verify an encrypted backup, finish
or cancel maintenance and replace the executable and assets together. Restore
that backup into a separate old-version installation for rollback. Earlier
v0.4.16 upgrades retain the v0.5.0 account, policy and helper requirements.

## Qualification

Implementation and archive evidence comes from preparation commit
`df0bae64b8e47302bac1ecbe68613028b129056b`; the following evidence/documentation
commit does not change runtime behavior. [PR #542](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/pull/542)
must pass [CI and CodeQL on its latest head](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/pull/542/checks).
Earlier implementation results do not replace that final-head gate.

| Check | Measured preparation result |
| --- | --- |
| Toolchain / Go quality | Go 1.26.10 alignment, `go vet ./...` and native build passed |
| Local Go suite | Full standard suite passed with canonical `TMPDIR=/private/tmp`, except the explicitly excluded Linux-awk-only hook test below |
| Frontend unit tests | **283 passed**, no skips or failures |
| CVE browser scenarios | **26 passed** without retries, including real-backend desktop/mobile workflows and verification-only retry |
| Complete local browser suite | **217 passed, 3 failed**; unrelated macOS limits and baseline reproduction are recorded below |
| Official v0.5.0 native compatibility | Checksum-verified Darwin arm64 archive; account/session, encrypted inventory, policies, trust, migration, restart, actual old encrypted-backup restore and restored restart passed |
| Independent negative control | Deliberately lost inventory was detected before an independent healthy run; fixtures cleaned up; **zero SSH contacts/new maintenance jobs** |
| Release metadata / notes | Exact workflow metadata and changelog extraction blocks passed for `RELEASE_TAG=v0.5.1`; no tag created |
| Five release archives | Exact workflow archive block built Linux amd64/arm64, Darwin amd64/arm64 and Windows amd64 from clean commit `df0bae6`; all checksums, formats and packaged README versions passed |
| Packaged native execution | The extracted Darwin arm64 archive with its own assets passed the independent migration/restart/restore scenario |
| Changed guide links | Local file targets verified; historical release records and pinned migration inputs retained |

See [sanitized native evidence](validation/cve-v051/native-upgrade.json),
[desktop](validation/cve-v051/desktop.png) and
[mobile](validation/cve-v051/mobile.png) captures. Screenshots contain synthetic
package evidence, a fixed fixture clock and a deliberately residual CVE.

## Confirmed workflow fixes

- The browser discarded a selected full-upgrade scope after a refused removal
  approval, silently showing standard upgrades. Preserve scope/removal drafts
  for the same Server, plan, digest and approval generation; reset on changes.
- Completed verification updated operation detail but left old whole-server
  counts and package rows. Reload current evidence after complete, partial or
  failed verification, independently of the immutable before/after transaction.
- Polling ignored changed queue/admission messages when the state stayed queued.
  Include message, verification attempts and approval identity in change detection.
- Temporary read errors outlived recovery. Clear recovered reads while retaining
  rejected-command feedback, including after an intervening read failure.

The new browser scenario was run red before each UI correction, then green at
1440px and 390px. It refuses an obsolete digest and unconfirmed removals before
observing exactly one correlated update and a distinct verification scan, one
corrected finding, one remaining finding, current-plan refresh and ledger state.
Existing runner tests cover all four scopes, source/repository/dependency drift,
partial proofs, cancellation, independent health failures and audit correlation.

## macOS validation limits

The initial full Go run encountered the macOS `/var` temporary-directory symlink;
using canonical `TMPDIR=/private/tmp` resolves the backup security refusals.
`TestDeferralHookRequiresCompleteApprovedTransaction/complete` also invokes the
Linux helper's absolute `/usr/bin/awk` and its `systime()` primitive, absent from
macOS awk. The local full-suite command excludes only that parent test with
`-skip '^TestDeferralHookRequiresCompleteApprovedTransaction$'`. Neither the
repository tests nor the helper were weakened; complete unfiltered Linux
race/coverage remain mandatory CI gates.

The full local browser run's archive-catalog failure passes with canonical
`TMPDIR`. Two delayed-load navigation cases (Manage and Admin) still fail on
this Mac and reproduce in an untouched `origin/main` extraction with one worker.
No unrelated navigation code/test is changed. All 26 CVE cases pass without
retries; the complete Linux browser suite must pass in final-head CI.

Native migration reuses `tools/ci/upgrade/scenario.py` through a private
adaptation for archive paths, run-owned loopback port/sentinel, preserved v0.5.0
sessions and additive remediation tables. Targets/policies stay disabled and
external HTTP uses an unreachable proxy. The report states the absence of
kernel network isolation; this is not a networkless-container proof. Private
credentials, databases, raw logs and encrypted archives are not committed.

## Scope of evidence

Browser scenarios use the production router, authentication, SQLite persistence,
maintenance runner and independent verification lifecycle in disposable test
installations. Host Maintenance Session and CVE provider transports use synthetic
fixtures: no managed server or public CVE service is contacted. Desktop and
mobile runs establish workflow behavior, not real-world fix availability or
runtime exploitability. Existing package-evidence tests cover partial scans,
source/provenance drift, cancelled work and independent health outcomes.

Docker is unavailable on this macOS workstation. Linux CI must qualify full Go
race/coverage, Docker amd64/arm64, the pinned published-v0.4.16 upgrade and
Debian 13/Ubuntu 24.04 SSH/APT fixtures. Those fixtures do not replace a live
external-provider or production deployment check. Do not relabel the v0.5.0
release evidence or an earlier extended-matrix run as qualification of this head.

## Before publication

Review this PR and its final CI/CodeQL results. Merge and publication require
separate authorization; after merge, recheck the exact main commit, tag ruleset
and all required release gates. The trusted release workflow must build and
qualify its own exact tagged archives and images before publication.
