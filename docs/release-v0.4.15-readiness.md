# v0.4.15 Release Readiness

Pre-tag preparation. Tag creation and publication are not part of this change.

## Candidate and scope

- Prepared: 2026-10-20 America/Toronto.
- Previous published version: `v0.4.14`.
- Implementation base: `200b7cb72f26a0c1051c505c25df40b2ef5e0531` on `main`.
- Preparation branch: `codex/prepare-v0.4.15`.
- Includes PR #459: independent scheduled autoremove and its review correction
  preserving scan-health history when cleanup succeeds or fails.
- README, UI version, installation/deployment/configuration examples and
  changelog are aligned to `v0.4.15`. The disposable-host release checklist now
  describes scheduled cleanup qualification.
- This preparation also synchronizes the cleanup browser regression with the
  completed save response. Production Go code, dependencies, schema and SSH
  helper are unchanged from the implementation base.

## Upgrade requirements

An upgrade from v0.4.14 requires no database schema migration, configuration
change or SSH helper update. Installations upgrading from older versions must
also follow the applicable earlier notes.

Cleanup is opt-in. Create a separate **Autoremove (automatic cleanup)** policy
with explicit server/tag targets, daily or weekly cadence and the application
timezone. Enabled cleanup removes unused dependency packages without per-run
approval. Existing scan/update policies retain their behavior; no cleanup policy
is created automatically. Package scope and upgrade mode do not apply to cleanup.
Global/policy no-run windows, overrides, disabled-server controls, maintenance
admission and optional staged rollouts still apply. Schedule cleanup apart from
updates or scans, which retain priority in coincident slots.

## Qualification

The exact implementation base passed [post-merge CI](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39436510702)
and [all four CodeQL analyses](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/actions/runs/39436512133).
The preparation PR must pass its own CI and CodeQL before merging.

| Check | Result |
| --- | --- |
| Canonical Go toolchain | Go 1.26.9 in `go.mod` and Docker builder; toolchain alignment passed on main |
| Full uncached Go race and atomic coverage suites | Passed on exact implementation base in CI; 74.5% aggregate coverage, above 73.0% |
| Vet, Staticcheck, Govulncheck and Actionlint | Passed on main; local preparation vet and native build also passed |
| Frontend unit tests / npm audit | 181 tests passed locally and on main; npm audit passed on main |
| Playwright candidate suite | 75 passed locally with retries disabled and Playwright 1.64.0 |
| Save/reset race reproduction | Controlled slow-response reproduction failed 3/3 before synchronization; minimized case passed 5/5 and full scenario passed 10/10 after synchronization, without retries |
| Workflow release metadata contract | Exact workflow block passed with `RELEASE_TAG=v0.4.15`, without creating a tag |
| Five workflow-format archives | Built with Go 1.26.9; final checksums/formats and packaged Linux amd64 startup are validated with the unchanged verifier |
| Docker amd64 and arm64 | Both passed startup, non-root PID 1, explicit non-root restart and persisted login |
| OS and Go binary container scans | Pinned Trivy 0.74.0 detected OS packages and the application binary on both architectures; zero HIGH/CRITICAL findings, including unfixed findings |
| Upgrade from published v0.4.14 / isolated backup restore | Passed on both architectures; account/session, encrypted credentials, disabled inventory/policy, timezone, metrics access, audit and host-trust state preserved; old backup verified/restored and destination sessions invalidated |
| Real scheduled autoremove via SSH | Passed on linux/arm64 with a disposable Debian target: actual scheduler occurrence removed only the synthetic unused package; audit/report identity verified and scan-health history unchanged |
| Hosted release tag protection | Active `Protect release tags v*` ruleset targets `refs/tags/v*` and prevents updates/deletions |

The successful main browser run classified two tests as flaky after a serial
group retry. Its actual failure was the new cleanup test editing the next draft
asynchronously with the previous save/reset; the setup-form test passed on its
first attempt and was skipped after the shared account already existed on retry.
The preparation waits for the existing `Policy created.` UI completion signal
before changing execution mode. The full candidate suite passed without retries;
no runtime behavior or timeout was changed to make the test pass.

Local Docker builds ran serially to stay within Docker Desktop's 2 GiB memory
limit. Both used fresh Alpine runtime package layers. Images remain local as
`slu-prep:v0.4.15-amd64` and `slu-prep:v0.4.15-arm64`; no GHCR write occurred.
Scanner results describe the vulnerability database at qualification time.

## v0.4.14 compatibility evidence

The previous image was resolved from published v0.4.14 `publication.json` and
pulled by immutable manifest digest
`sha256:ca23fcc7f3cedaf13097d85983f426c1d7f298c9fe64a02854c22b9bd5f23715`,
for commit `d7e7caf281f1f12f480e375761f22d0a9d724976`. Each architecture used
separate disposable volumes, generated credentials and a disabled inventory
fixture. No existing application database or saved host was used.

The candidate reopened the same volume explicitly as `app`; authenticated
sessions and encrypted credentials remained usable. Offline snapshots included
WAL sidecars; database integrity passed, SQL schema definitions were identical,
and account, inventory and policy rows were unchanged. A backup exported by
v0.4.14 verified and restored into another disposable candidate instance on each
architecture. Restored account login succeeded after destination sessions were
invalidated. Temporary upgrade containers and volumes were removed.

## Scope of live verification

Scheduled-cleanup qualification uses a new root/per-server-key SSH identity and
a synthetic package marked as automatically installed on a disposable Debian
container. Before enabling its policy, APT simulation must identify only that
fixture as removable. The smoke checks the actual scheduler, package removal,
persisted job/run identity, report/audit output and unchanged scan-health history.
The pass completed on linux/arm64. Recorded SSH commands contained no APT update
or upgrade. The fixture file/package was gone, both completion audit events
retained the scheduled identity, and offline health-history rows were unchanged.
The target, app container, network and volume were removed afterward.

Archive validation executes the unchanged Linux amd64 probe inside a disposable
container on the macOS arm64 host. Darwin and Windows archives are checked for
format and checksum; their binaries are not executed on native runners.

Other credential modes, failures, no-run windows, busy/disabled admission and
canary/wave behavior are covered by automated tests; no new multi-host live
rollout is claimed. External notification delivery and a controlled reboot are
not repeated. The disposable container has no real systemd boot environment.

Local scripts, logs, scan reports and generated fixtures are retained outside
tracked files under `/private/tmp/slu-v0415-qualification/`. Fixture secrets are
not part of this preparation. Local qualification artifacts are not publication
bytes; the trusted release workflow must qualify its own final artifacts.

## Before tagging

1. Review and merge the preparation PR after its CI and CodeQL succeed.
2. Verify final CI and CodeQL on the exact merged `main` commit.
3. Obtain explicit authorization to tag and publish `v0.4.15`.

Publication must use the existing tag-signal/trusted-release workflow. Verify
its final archives/checksums, `publication.json`, both image platforms,
version/latest digest identity and the distributed-image audit after publication.
