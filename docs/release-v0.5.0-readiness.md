# v0.5.0 Release Readiness

Pre-tag preparation. This branch creates no tag, GitHub Release or registry image.

## Candidate and scope

- Prepared: 2026-10-29 America/Toronto.
- Previous published version: `v0.4.16` (`60432cac60d08722cdf0034a000d059a81ee967d`).
- Implementation base: `6da93c7cd16e09a0e8049d71c61735a09548df8f` on `main`.
- Preparation branch: `codex/prepare-v0.5.0`.
- README, displayed version, installation/deployment/configuration examples and
  changelog are aligned to `v0.5.0`.
- Preparation includes version/documentation updates and targeted release fixes:
  the BSD recovery input-flush mask, a macOS compile check in CI, first-connection
  SSH diagnostics in the VM harness, and deterministic cancellation coverage in
  the browser harness. Database migrations, SSH helper and dependencies retain
  the implementation base's behavior. SQLite remains at `v1.60.1`; PR #510 is
  not part of this release.
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

The application baseline is `6da93c7`, with the qualification and Darwin compile
fixes in `7f8452e` and `f3320e8`. Final evidence/documentation commits do not add
runtime behavior. [PR #525](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/pull/525)
must pass its own [CI and CodeQL checks](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/pull/525/checks)
on the final head before merge. The exact merged `main` must pass again before
any tag is created. Earlier CI runs and the previous release's records do not
replace either requirement.

| Check | Measured preparation result |
| --- | --- |
| Toolchain and dependencies | Go 1.26.10 matches Docker; module checksums verified; no dependency version changes |
| Go standard suite | `go test -count=1 ./...` passed |
| Race suite | `GOMAXPROCS=2 go test -race -count=1 -p=1 -timeout=20m ./...` passed; package concurrency bounded on this runner |
| Atomic coverage | Full suite passed; **76.7%** aggregate versus the **73.0%** minimum; versioned coverage check passed |
| Go and workflow quality | Vet, Staticcheck v0.7.0, Govulncheck v1.3.0, Actionlint v1.7.12 and static native build passed |
| npm audit and unit tests | Zero npm findings; **267** frontend unit tests passed |
| Browser suite | **192** Playwright scenarios passed with two workers and **retries disabled**; cancellation regression also passed five repeated runs |
| Metadata and release notes | Exact version-validation and changelog-extraction workflow blocks passed with `RELEASE_TAG=v0.5.0`; this environment value did not create a tag |
| Release archives | Exact archive workflow block built Linux amd64/arm64, Darwin amd64/arm64 and Windows amd64; all checksums and formats passed; the packaged Linux amd64 executable started with isolated data |
| Docker amd64 | Fresh runtime packages; startup, non-root PID 1, explicit non-root persistent restart, administrator recovery and health-probe cases passed |
| Docker arm64 | Built the unchanged Dockerfile with BuildKit's bundled user-mode emulation; no host binfmt registration; native/emulated runtime smoke remains a hosted CI gate |
| Container security | Pinned Trivy 0.74.0 scanned actual OS packages and Go binaries for **both** local image architectures; **zero HIGH/CRITICAL** findings, with no unfixed-finding suppression |
| Published v0.4.16 compatibility | Pinned archive/checksum/provenance verification, migration, restart continuity, real old export/candidate restore and cleanup passed; injected inventory loss was detected by an independent negative control |
| Real SSH and APT | Debian 12/13 and Ubuntu 22.04/24.04/26.04 each passed seven production scenarios; root/operator upgrade, trust, obsolete approval, locks and interrupted-mutation reconciliation/repair were exercised |
| Package Deferral guards | All five distributions passed twelve healthy source-generated guard scenarios; every injected missing-action regression was detected before the fresh healthy helper passed |
| Managed helper distribution | **16** scenarios on each of Debian 13 and Ubuntu 24.04 passed, including non-root onboarding, exported installers, legacy/malformed refusals, atomicity and temporary-file cleanup |
| Additional production integration | **14** groups passed across isolated API, backup/notification and real HTTP post-check runs; see scope below |
| Real systemd/reboot | Fresh Debian 12 QEMU guest passed real SSH service health and controlled reboot; changed guest boot ID, reduced uptime, succeeded job/audit and cleanup were verified |
| Hosted release tag protection | Active ruleset **22057215**, `Protect release tags v*`, covers `refs/tags/v*`, forbids updates/deletions and has no bypass actors |
| Documentation | Current image/version examples and local Markdown links in the changed guides were checked; historical records and the v0.4.16 migration pins remain intact |

## Additional production integration scope

Fresh production binaries and generated installations exercised authenticated
body budgets, atomic administration of 1,000 disabled fixture Servers, restart
persistence and conflicting-preview refusal; inventory export/import round trips
and filtered audit exports; automated archive catalog identity, download checksum,
reverification and tamper refusal; and liveness/readiness, protected diagnostics
and metrics. Documentation-only addresses and disabled inventory prevented
contact with managed hosts.

Separate production instances verified real encrypted backup export, verify,
restore and storage failure/retention behavior. Notifications went only to a
run-owned loopback HTTP receiver, including condition admission and deduplication
across restart. Real fixture APT upgrades exercised deliberately failing HTTP
post-checks in both blocking and warning modes with job/audit assertions. No
Discord, Telegram or external webhook recipient was contacted.

The published-version harness uses the pinned v0.4.16 Linux amd64 archive and
real production endpoints, not a reconstructed legacy database or a stubbed
restore. It confines both versions to a networkless container and publishes only
redacted scenario reports. Its cleanup passed after the expected negative
failure and after the independent positive run.

## Findings resolved during preparation

- The initial Darwin archive build failed because `unix.FREAD` is not exported
  by the current Go binding. The recovery ioctl now uses the documented BSD
  read mask (`0x01`), preserving input-only flushing. Both Darwin architectures
  compile, all five archives build, and CI now checks Darwin amd64. The mask
  follows [Apple's kernel header](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/fcntl.h).
- The first VM run correctly refused an ownership check when the first SSH
  connection's accepted-host diagnostic was combined with the marker output.
  `LogLevel=ERROR` suppresses that informational text while retaining identity
  validation and SSH errors. The regression rejects the old harness, 37 harness
  tests pass, and a fresh VM rerun completed the real reboot successfully.
- A browser cancellation scenario relied on a 300 ms response delay and could
  lose its cancellation window under load. An explicit response gate now holds
  the response until after cancellation, then verifies that the late response
  cannot reveal the IP. Five repeated checks and the complete 192-test suite pass.
- The first local race run shared the runner with simultaneous cross-compilation,
  image and browser work, exceeded its global ten-minute limit and hit an audit
  fixture deadline. The full rerun bounded CPU/package concurrency and passed;
  individual assertions were not removed or relaxed. Final hosted CI retains
  its normal independent race job.

## Security scan interpretation

Govulncheck found zero reachable symbol vulnerabilities and zero vulnerabilities
in imported packages. Its verbose module inventory also lists
[GO-2026-6866](https://pkg.go.dev/vuln/GO-2026-6866) and
[GO-2026-6860](https://pkg.go.dev/vuln/GO-2026-6860) in unused `x/net/html` code,
and [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) for unused
`x/crypto/openpgp`. This is not a claim that every required module is free of
advisories. The separate pinned image scans found no HIGH/CRITICAL OS or Go
binary findings on either architecture at preparation time. Publication must
refresh its own scans against the final artifacts and vulnerability database.

## Evidence and limits

Private logs, generated credentials, cloud-init seeds, databases and raw browser
traces remain outside tracked files under the preparation runner's temporary
`slu-release-v0.5.0-evidence` directory. No target credentials, backup secrets,
application databases or raw traces are committed. Fixtures use their own
ownership labels and cleanup; no existing inventory or host package manager is
used. The temporary arm64 BuildKit builder was removed after qualification.

The VM used a cached official Debian 12 cloud image verified against its recorded
official SHA512 checksum, with SHA256
`b1b018dc1046fb21815f6cfe4b67ac1d56bc841e19c4ba2c16073031386ad774`.
QEMU tools ran in an unprivileged container with only the generated VM working
directory writable; no Docker socket, host PID namespace, privileged capability
or KVM device was mounted into that tools container. The guest ran its own kernel
with TCG and the application connected only to its allocated loopback SSH socket.

Containers cannot establish systemd/reboot behavior or official distribution
CVE provenance; those three container report entries remain explicitly
`not_executed`. The real Debian VM covers systemd/reboot separately. Live external
CVE-provider availability and a full OS/kernel package upgrade were not qualified;
APT transactions used only synthetic fixture packages after public bootstrap
sources were removed. Provider interpretation and failure behavior retain their
versioned unit/integration coverage.

The Docker migration proof uses the official old **archive** in the pinned
runtime, not the old GHCR image or every operator's Compose/proxy deployment.
Darwin/Windows binaries were compiled and archive-checked, not executed natively;
interactive Darwin terminal behavior was not exercised on this Linux runner.
Local arm64 image execution was not available; the final PR's two-architecture
Docker smoke is required. Native macOS/Windows deployment, external notification
transports and live external provider availability are not implied by these tests.

No tag, public release, registry push, digest promotion or publication write is
part of preparation. The trusted release workflow must rebuild and qualify its
own exact tagged artifacts; local archives/images are review evidence only.

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
