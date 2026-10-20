# v0.4.14 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-10-19 America/Toronto.
- Previous published version: `v0.4.13`.
- Implementation base: `c801c99bbbb3639697480c4bc8f376eb79e9ba53` on `main`.
- Preparation branch: `codex/prepare-v0.4.14`.
- Includes PR #452 (verified SSH server onboarding), #453 (Go modules),
  #454 (Playwright), #455 (setup-node), #456 (upload-artifact), and #457
  (restore regression fixture stability).
- Go is pinned to 1.26.9 in `go.mod` and the Docker builder. The release includes
  `golang.org/x/crypto` 0.58.0 and `golang.org/x/net` 0.61.0.
- README, UI version, installation/deployment/configuration examples and
  changelog are aligned to `v0.4.14`. Usage and release smoke instructions
  describe the verified Add Server assistant.

## Upgrade requirements

An upgrade from v0.4.13 requires no database schema migration, configuration
change, new SSH helper installation, or API approval changes. Installations
upgrading from older versions must also follow the applicable earlier notes.

New UI server creation requires verified SSH access, an explicitly confirmed
host fingerprint, and prerequisite checks. Non-root targets need the existing
managed helper and passwordless maintenance permissions provisioned beforehand;
onboarding does not install them. Existing inventory entries and the compatible
`POST /api/servers` inventory-import API remain available.

## Qualification

The exact implementation base passed [post-merge CI](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39362723484)
and [all four CodeQL analyses](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39362723443)
after #457 was merged. This preparation changes release metadata and documentation,
including the displayed UI version; production Go sources and dependencies are
unchanged from that base.

| Check | Result |
| --- | --- |
| Canonical Go toolchain | Go 1.26.9; workflow toolchain alignment passed |
| Full uncached race and atomic coverage suites | Passed on the exact implementation base in CI; 74.4% aggregate coverage, above 73.0% |
| Vet, Staticcheck, Govulncheck and Actionlint | Passed in CI; local vet and native build also passed with Go 1.26.9 |
| Source vulnerability analysis | No called or imported-package vulnerabilities; one required-module finding without affected calls |
| Frontend unit tests / npm audit | 180 passed locally and in CI; zero npm vulnerabilities |
| Playwright browser tests | 74 passed locally with Playwright 1.64.0 and in CI |
| Workflow release metadata contract | Exact workflow block passed with `RELEASE_TAG=v0.4.14`, without creating a tag |
| Five workflow-format release archives | Built with Go 1.26.9; unchanged verifier passed checksums/formats and started the packaged Linux amd64 binary |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Pinned Trivy 0.74.0 detected OS packages and the application binary on both architectures; zero HIGH/CRITICAL findings, including unfixed findings |
| Upgrade from published v0.4.13 / isolated backup restore | Passed on amd64 and arm64; persisted data, sessions and credentials retained; old backups verified/restored and destination sessions invalidated |
| Real SSH onboarding | Passed through the candidate arm64 API against a disposable Debian 12 SSH target using a per-server key |
| Hosted release tag protection | Active `Protect release tags v*` ruleset targets `refs/tags/v*` and prevents updates/deletions |

The intermittent restore preparation-failure test was fixed in #457 by matching
the production single-connection SQLite pool and forcing notification retry
overlap. The focused failure path passed 100 macOS repetitions with the race
detector and 100 Linux repetitions. The full hosted race suite passed again on
merged `main`. Production restore behavior was not changed by that test fix.

Local Docker qualification used Docker Desktop 29.9.1 with fresh Alpine 3.24.2
runtime package layers. Building both architectures together exceeded the local
2 GiB Docker memory limit; serial builds succeeded and were assembled into the
local amd64/arm64 manifest list
`sha256:b31fda751445b30f6e001b5a95b93019ceae59a701e64819d28f819d8a51c311`.
Nothing was pushed to GHCR. The implementation base also passed the hosted Docker
28.0.4/containerd smoke path. The preparation PR must pass its own CI and CodeQL.
Scans describe the vulnerability database at qualification time.

Archive qualification ran the unchanged verifier in a disposable Linux amd64
container, with its required `unzip` tool installed, on a macOS arm64 host.
Darwin and Windows archives passed format/checksum checks; their packaged
binaries were not executed on native runners.

## v0.4.13 compatibility evidence

The previous image was resolved from the published v0.4.13 `publication.json`
and pulled by its immutable manifest digest,
`sha256:8938073cea8b8d361e709459ed92af88f6f761b5ec99741245edcf10b9b5547b`,
for commit `656fad8334645e8816bed6d70dfb33551f999fc2`. Each architecture used
separate disposable volumes, generated credentials, and an offline disabled
inventory fixture. No existing application database or saved host was used.

On both architectures, the candidate reopened the same volume explicitly as
`app`. It retained the authenticated session, encrypted password and SSH key,
disabled Server Inventory entry, disabled scan policy, timezone, Metrics Access
Credential, pre-upgrade audit records, encryption configuration and
`known_hosts`. Offline snapshots included WAL sidecars. Integrity checks passed,
SQL schema definitions were identical, and persisted account, inventory and
policy rows were unchanged.

A backup exported by v0.4.13 was verified, then restored into another disposable
v0.4.14 instance on each architecture. Destination sessions were invalidated;
login with the restored account succeeded, and the disabled server, policy,
timezone and host-trust file were retained.

## SSH qualification and limits

The disposable target fingerprint was read from its console and pinned for an
independent SSH login and read-only `apt-get -s upgrade` reachability check.
Through the candidate API, an incorrect fingerprint was rejected; successful
draft verification left inventory and host trust empty. An invalid credential
submitted after verification was rejected at creation. Verified creation saved
the inventory entry and encrypted per-server key, pinned host trust and recorded
`onboarding_verified` in the audit trail. The target's dpkg database and managed
helper state were unchanged by these checks.

This live pass exercised root/per-server-key onboarding. Other credential modes,
non-root prerequisites and failure/retry UI states are covered by the automated
suites, without claiming a live pass for those modes. Live package mutation,
scheduled-policy execution and notification delivery were not repeated; those
application flows are unchanged by this release. Controlled reboot was skipped
because the disposable target is a container without a real systemd boot
environment. Temporary containers, networks and volumes created by qualification
were removed.

Local scripts, logs, reports and generated fixtures are retained outside tracked
files under `/private/tmp/slu-v0414-qualification/`. Fixture secrets are not part
of this PR. Local preparation artifacts are not publication bytes; the trusted
release workflow must qualify its own final artifacts.

## Before tagging

1. Review and merge the preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.14`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
its final archives/checksums, `publication.json`, both image platforms,
version/latest digest identity and the distributed-image audit after publication.
The older v0.4.13 distributed-image audit is not evidence about the newly scanned
candidate or about future publication bytes.
