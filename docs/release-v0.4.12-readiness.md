# v0.4.12 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-09-20 America/Toronto.
- Previous published version: `v0.4.11`.
- Implementation base: `876e4ceb8861337b2e5bc1b46fb60542ecfdcf36` on `main`.
- Preparation branch: `codex/prepare-v0.4.12`.
- Includes PRs #441–#444: shorter diagnostic-artifact retention, corrected
  backup size/deadline boundaries, atomic host-trust updates, Go dependency
  updates, and Status-page readability and action-visibility improvements.
- README, UI version, deployment examples, changelog, and release documentation
  are updated for `v0.4.12`.

## Upgrade requirements

An upgrade from v0.4.11 does not require another SSH helper installation, a
configuration migration, or a new API approval contract. Installations upgrading
from older versions must also follow the applicable earlier upgrade notes.

## Qualification

| Check | Result |
| --- | --- |
| Exact Go toolchain | Go 1.26.6 from `go.mod`; release tools rebuilt with that toolchain |
| Full uncached race and atomic coverage suites | Passed; 74.3% aggregate coverage, above the 73.0% minimum |
| Vet, Staticcheck, Govulncheck, Actionlint and native build | Passed; no called or imported-package vulnerabilities; one required-module finding without affected calls |
| Frontend unit tests / npm audit | 177 passed; zero npm vulnerabilities |
| Playwright browser tests | 73 passed |
| Workflow release metadata contract | Passed with `RELEASE_TAG=v0.4.12`, without creating a tag |
| Five workflow-format release archives | Built with Go 1.26.6; archive integrity and SHA-256 checksums passed |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Both passed with pinned Trivy 0.74.0; zero HIGH/CRITICAL findings |

The release archive runtime probe is Linux-only. The local macOS host verified
all archive checksums and formats but cannot directly execute the packaged Linux
amd64 ELF binary; the trusted Linux release runner must execute that unchanged
probe before publication. Live disposable-host SSH maintenance, scheduled policy,
backup migration/restore, notification delivery, and controlled reboot were not
repeated for this preparation. The affected backup, host-trust, lifecycle and
Status-page behavior is covered by the full Go and browser suites above.

Local Docker qualification used fresh Alpine runtime package layers. The amd64
image manifest list was
`sha256:c8e840a092dc04d76d0e24133588b329e0b72ad776bc95cc29bd45c9dfd68abc`;
the arm64 image manifest list was
`sha256:0debd587ead622c2654517f438ec000430de805e74fd67bc89806843dd1103cc`.
Neither image was pushed to GHCR. Scan results describe the vulnerability
database at qualification time.

Local preparation artifacts are not publication bytes; the trusted release
workflow must qualify its own final artifacts.

## Before tagging

1. Review and merge the preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.12`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
archives/checksums, `publication.json`, both image platforms, version/latest
digest identity and the distributed-image audit after publication. Local
qualification does not establish that those future publication steps passed.
