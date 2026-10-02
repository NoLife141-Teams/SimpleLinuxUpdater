# v0.4.13 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-10-02 America/Toronto.
- Previous published version: `v0.4.12`.
- Implementation base: `67445f0692efae168cab59db6787bdc8a9113843` on `main`.
- Preparation branch: `codex/prepare-v0.4.13`.
- Includes PRs #449, #447, #450, #448, and #446: pinned Docker action updates,
  SQLite driver 1.60.1, and its matching libc 1.77.1 dependency.
- The embedded SQLite version stays 3.53.4. Upstream fixes cover WAL
  shared-memory read faults and binding statements with many parameters;
  upstream runtime optimizations are not measured application speedups.
- README, UI version, deployment examples, changelog, and release documentation
  are updated for `v0.4.13`.

## Upgrade requirements

An upgrade from v0.4.12 does not require another SSH helper installation, a
configuration or database schema migration, or a new API approval contract.
Installations upgrading from older versions must also follow the applicable
earlier upgrade notes.

## Qualification

| Check | Result |
| --- | --- |
| Exact Go toolchain | Go 1.26.6 from `go.mod`; release tools rebuilt with that toolchain |
| Full uncached race and atomic coverage suites | Passed; 74.3% aggregate coverage, above the 73.0% minimum |
| Vet, Staticcheck, Govulncheck, Actionlint and native build | Passed; no called or imported-package vulnerabilities; one required-module finding without affected calls |
| Frontend unit tests / npm audit | 177 passed; zero npm vulnerabilities |
| Playwright browser tests | 73 passed |
| Workflow release metadata contract | Exact workflow block passed with `RELEASE_TAG=v0.4.13`, without creating a tag |
| Five workflow-format release archives | Built with Go 1.26.6; unchanged archive verifier passed all checksums and formats and started the packaged Linux amd64 binary in a disposable Linux container |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Both passed with pinned Trivy 0.74.0; zero HIGH/CRITICAL findings, with OS packages and the application binary detected |
| Upgrade from published v0.4.12 / isolated backup restore | Passed on amd64 and arm64; persisted data, existing session and credentials retained, old backups verified/restored, destination sessions invalidated |
| Hosted release tag protection | Active `Protect release tags v*` ruleset, targeting `refs/tags/v*` and preventing updates/deletions |

Local Docker qualification used Docker Desktop 29.8.0 and fresh Alpine runtime
package layers. The local candidate amd64/arm64 manifest list is
`sha256:75079921e4e3ec2bcfbe7583577c6fcf4f2247c61cd8744f2e2fcb2d88215657`.
It was not pushed to GHCR. The preparation PR must also pass the hosted Docker
28.0.4/containerd smoke path, which executes the newly pinned setup actions.
Scan results describe the vulnerability database at qualification time.

Archive qualification executed the repository's unchanged Linux archive probe
inside a disposable Linux amd64 container on the local macOS arm64 host. The
Darwin and Windows archives passed format and checksum checks; their packaged
binaries were not executed on native runners. Live disposable-host SSH
maintenance, scheduled-policy execution, notification delivery and controlled
reboot were not repeated. This maintenance changes dependencies and publication
tooling; it does not change those application flows.

Local artifacts, logs and scan reports are retained outside tracked files under
`.tmp-smoke/release-v0.4.13/`. Local preparation artifacts are not publication
bytes; the trusted release workflow must qualify its own final artifacts.

## v0.4.12 compatibility evidence

The old image was resolved from the published v0.4.12 `publication.json` and
pulled by its immutable manifest digest,
`sha256:d3b97d57074a2bcaced2d8c07230d2f373151fde438b49bce3cc2cf2704d15b9`,
for commit `142d8650fa264abb83bfcfc734fe2b7331df1683`. Each architecture used
separate disposable volumes, generated credentials and an offline disabled
inventory fixture; no saved host or existing application database was used.

For both architectures, the new image reopened the same volume explicitly as
`app`. It retained the existing authenticated session, encrypted password and
SSH key, disabled Server Inventory entry, disabled scan policy, timezone,
Metrics Access Credential, pre-upgrade audit records, encryption configuration
and `known_hosts`. Offline snapshots include the WAL sidecars; integrity checks
passed, SQL schema definitions were identical, and persisted account, inventory
and policy rows were unchanged.

A backup exported by v0.4.12 was accepted by v0.4.13 verification, then restored
into another disposable v0.4.13 instance on each architecture. The destination
session was invalidated; login with the restored account succeeded, and the
disabled server, policy, timezone and host-trust file were retained. Temporary
containers and volumes were removed after qualification. The reproducible local
harness and sanitized result summary remain with the ignored preparation
artifacts; generated fixture secrets are not part of the preparation PR.

## Before tagging

1. Review and merge the preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.13`.

Publication must use the existing tag-signal/trusted-release workflow. Its
runner must qualify its own final archives and image bytes. Verify
archives/checksums, `publication.json`, both image platforms, version/latest
digest identity and the distributed-image audit after publication. Local
qualification does not establish that those future publication steps passed.
