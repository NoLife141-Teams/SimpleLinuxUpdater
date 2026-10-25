# v0.4.16 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-10-23 America/Toronto.
- Previous published version: `v0.4.15`.
- Implementation: PR #462, approval-plan drift and Go security patch.
- Implementation base: `0f4e83e3d6e5ea20eaa0924f898580e5fbff847d` on `main`.
- Preparation branch: `codex/prepare-v0.4.16`.
- README, displayed UI version, installation/deployment/configuration examples,
  and changelog are aligned to `v0.4.16`.
- This preparation changes release metadata and documentation only. Application
  Go code, dependencies, database schema and the SSH helper retain the
  implementation base's behavior.

## Upgrade requirements

An upgrade from v0.4.15 requires no schema migration, configuration change,
SSH helper installation or API request change. Installations upgrading from
older versions must follow the applicable earlier notes.

Approval remains bound to the displayed server, job and plan generation. When
selected package execution facts change after approval, the refreshed plan
requires renewed consent. This includes version and repository-source changes
that keep the same package name. CVE-only asynchronous enrichment does not
require renewed consent. Existing package-removal restrictions remain in force.

The security correction protects approval integrity in the documented
single-admin LAN/VPN model. Its reproduction requires metadata to change while
approval is pending; it does not demonstrate unauthenticated code execution.
The original Go 1.26.9 toolchain contained reachable affected `crypto/x509` code;
an application-specific exploit of GO-2026-6809 was not demonstrated.

## Qualification

The pre-synchronization implementation and its exact merge-candidate tree passed
[PR CI](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39968057707)
and all four CodeQL analyses. Two independent review passes concluded
`No findings.` The branch was then synchronized with `main` without changing
that tested tree (`c024d71f5ddbf7d66d35d537326b9063171cac63`), as required by the
repository's protected-branch checks.

The synchronized branch passed [PR CI](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39972459486)
and all four CodeQL analyses before PR #462 was merged as `0f4e83e`. The
preparation PR and final merged release commit must pass their own CI/CodeQL
checks before tagging.

The merged implementation also passed
[post-merge CI](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39973074533)
and all four CodeQL analyses on `0f4e83e`.

| Check | Result |
| --- | --- |
| Canonical Go toolchain | Go 1.26.10 in `go.mod` and Docker builder; implementation alignment passed |
| Go tests, race and aggregate atomic coverage | Implementation candidate passed; 74.5% aggregate coverage, above 73.0% |
| Vet, Staticcheck, Govulncheck and Actionlint | Implementation candidate and merged main passed; preparation vet and native build passed locally; zero reachable or imported-package vulnerabilities |
| Frontend unit tests / npm audit | 181 passed on implementation candidate; zero npm vulnerabilities |
| Playwright implementation candidate | 75 passed locally with retries disabled and in PR CI |
| Workflow release metadata | Exact workflow block passed with `RELEASE_TAG=v0.4.16`; no tag was created |
| Five workflow-format release archives | Built with Go 1.26.10; unchanged verifier passed all checksums/formats and packaged Linux amd64 startup |
| Disposable-host security and functional checks | 32 Docker functional check groups and six Debian VM check groups passed |
| Controlled reboot and systemd | Independent Debian 12 guest kernel rebooted through the app; guest boot ID changed, VPS boot ID unchanged; healthy postchecks and a deliberately newly failed unit were both detected correctly |
| Published v0.4.15 compatibility | Passed on amd64 with dedicated volumes: account/session, three SSH credential modes, global key, disabled inventory/policy, timezone, metrics and host trust preserved; old encrypted backup verified/restored and destination session invalidated |
| Docker amd64 | Passed startup, non-root PID 1, explicit non-root restart and persisted login |
| Container vulnerability scan | Pinned Trivy 0.74.0 detected OS packages and the Go application binary on local amd64; zero HIGH/CRITICAL findings, including unfixed findings; release workflow must requalify its own artifacts |
| Hosted release tag protection | Verified active ruleset `22057215`, `Protect release tags v*`, covering `refs/tags/v*`, with update/deletion restrictions and no bypass actors |

## v0.4.15 compatibility evidence

The previous image was resolved from the published v0.4.15 `publication.json`
and pulled by immutable manifest digest
`sha256:17faafafad94f8f7db38ff48f93cb20ea3c65c3571d2c066b2c16e0b1fe2d156`,
for commit `7cd91466d86d74c5a347a56a1e68653723082f8a`.
New temporary volumes, generated account/backup credentials and three disabled
inventory entries used only the existing disposable SSH fixture. SSH host
fingerprints were checked independently through the fixture container console.

The candidate reopened the v0.4.15 volume explicitly as `app`. Its existing
authenticated session, subsequent login, metrics token, timezone, disabled
policy and inventory survived. Password, per-server key and global-key fallback
each performed a read-only SSH facts refresh successfully before and after
the upgrade. Each target was immediately disabled after its probe.

A backup exported by published v0.4.15 verified and restored into a separate
fresh candidate volume. The destination sentinel account/session was replaced
and revoked, restored login succeeded, and all three SSH credential modes
worked after rewrapping. Offline SQLite snapshots passed integrity checks;
schema definitions matched before upgrade, after upgrade and after restore.
Account, inventory and policy rows were unchanged on reopening the old volume,
before the explicit probe writes that renew encrypted credential nonces.
All compatibility containers and volumes were removed after qualification.

## Live verification and limits

Security qualification used fresh SQLite, configuration and `known_hosts` files,
fake credentials and dedicated disposable SSH targets. Automatic application
policies stayed disabled, and notifications used a local receiver. Package
operations used synthetic packages from a local repository. No APT operation,
sudoers modification or reboot ran on the VPS itself or an existing managed
server.

The VM ran QEMU TCG inside an unprivileged Docker container, without KVM, host
ports, host PID namespace or privileged capabilities. Its own Linux kernel and
systemd allowed a genuine controlled-reboot check. It was disabled in the app
and powered off after qualification.

Ubuntu, a full OS/kernel package upgrade, actual repair after an ambiguous
interrupted mutation, and a new multi-host rollout were not exercised live.
The residual interval between final metadata revalidation and APT execution is
not closed by version pinning; no exploit was reproduced for that interval.
An existing 5 px Admin document overflow at a 390 px viewport remains cosmetic
and is outside this security maintenance change.

Local qualification artifacts, credentials and full diagnostic logs stay
outside tracked files under `/tmp/slu-release-v0.4.16-qualification/` and the
private dev evidence directory. Preparation artifacts are not publication bytes. The
trusted release workflow must build and qualify the exact final tag before
promoting any image or publishing a GitHub Release.

The VPS builder supports amd64 only. Local arm64 image execution, upgrade and
vulnerability scanning were not repeated; no privileged binfmt registration
was installed on the host. The implementation's hosted Docker smoke passed
both amd64 and arm64, and the trusted release workflow must qualify both
platforms again. Darwin, Windows and Linux arm64 archives were cross-built and
checked for format/checksum; their binaries were not executed locally.

## Before tagging

1. Review and merge the preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Verify the active `Protect release tags v*` ruleset and complete any remaining
   qualification listed above.
4. Obtain explicit authorization to tag and publish `v0.4.16`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
its final archives/checksums, `publication.json`, both image platforms,
version/latest digest identity and the distributed-image audit after publication.
