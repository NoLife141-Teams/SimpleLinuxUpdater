# v0.4.8 Release Readiness

This record tracks pre-tag validation for `v0.4.8`. A tag may be created only
from the final merged `main` commit after its required CI is green.

## Candidate

- Prepared: 2026-09-03 America/Toronto
- Release version: `v0.4.8`
- Previous release: `v0.4.7`
- Release implementation base: `b8f36e1`
- Release preparation branch: `release/v0.4.8`
- Release tag: not created

## Scope

- Replace generic passwordless APT grants with a typed, root-owned helper and
  conservative legacy sudoers migration.
- Harden release provenance and align the release gate with normal CI.
- Refresh host facts automatically and through successful scheduled scans.
- Make uncertain APT outcome reconciliation depend on explicit command effects.
- Fix native Windows/Unix known-hosts path lists, listener defaults and
  validation, and normalized SSH endpoint uniqueness.
- Align the Go toolchain, add a coverage regression guard, update Go security
  dependencies, and split focused Go/frontend transport responsibilities.

## Release-Critical Compatibility

- Non-root hosts using the pre-v0.4.8 `/etc/sudoers.d/apt-nopasswd` policy need
  one **Enable apt** action. Exact app-generated legacy rules migrate to the
  typed `/usr/local/sbin/simplelinuxupdater-root-helper` and
  `/etc/sudoers.d/simplelinuxupdater`; ambiguous or administrator-managed files
  are refused without overwrite or deletion.
- Direct binaries now default to `127.0.0.1:8080`. Docker keeps the established
  published-port behavior by setting `DEBIAN_UPDATER_LISTEN_ADDR=:8080` in the
  image.

## Pre-Tag Automated Gate

The following local results passed against this branch. The Go test and race
suites were also repeated with `GOTOOLCHAIN=go1.26.6`, matching CI and Docker:

- [x] `go test -count=1 ./...`
- [x] `go test -race -count=1 ./...`
- [x] `go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...`
- [x] `go tool cover -func=coverage.out`: 73.3% total with Go 1.26.6, above the
  73.0% floor (the host Go 1.27.0 run measured 74.0%)
- [x] `go vet ./...`
- [x] `staticcheck ./...` with v0.7.0
- [x] `govulncheck ./...` with v1.3.0: zero reachable vulnerabilities
- [x] `go build -o webserver .`
- [x] Release-equivalent CGO-disabled builds for Linux amd64/arm64, macOS
  amd64/arm64, and Windows amd64
- [x] `npm ci`
- [x] `npm run test:unit`: 170 passed
- [x] `npm audit --audit-level=moderate`: zero vulnerabilities
- [x] `npm run test:e2e`: 67 passed
- [x] `actionlint` v1.7.12
- [x] `tools/ci/verify-go-toolchain.sh`: Go 1.26.6 aligned
- [x] Release lineage and workflow architecture tests, including rejection of
  mismatched signals and commits outside `main`
- [x] `git diff --check`

## Docker Runtime Gate

- [ ] Build the exact branch candidate image.
- [ ] Verify the final process is non-root.
- [ ] Verify `/data` is writable and SQLite/config state persists across restart.
- [ ] Verify the image explicitly listens on `:8080` and the UI is reachable.
- [ ] Complete first-run setup and login against a fresh disposable database.

The Docker runtime gate is pending because both available remote daemons timed
out over SSH and the local default socket does not exist. No Docker context was
changed and no substitute image was published.

## Security and Host-Maintenance Gate

- [x] Confirm `golang.org/x/crypto` v0.56.0 is the expected release dependency.
- [x] Confirm `govulncheck` reports no reachable vulnerability.
- [x] Prove through focused helper/sudoers tests that the generated managed rule
  invokes `/usr/local/sbin/simplelinuxupdater-root-helper` only and contains no
  generic `NOPASSWD` grant for `/usr/bin/apt` or `/usr/bin/apt-get`.
- [x] Prove through focused escape tests that arbitrary options, APT hooks,
  shell metacharacters, command substitution, paths, removal/pattern selectors,
  invalid architectures, and unknown operations are refused before dispatch.
- [x] Prove through focused guard tests that managed and legacy paths are checked
  before mutation and unrecognized legacy content is refused.
- [ ] Exercise typed APT update, upgrade/full-upgrade selection, autoremove,
  repair/lock probes, selected packages, and controlled reboot where the
  disposable target can safely prove them.
- [ ] Exercise exact legacy sudoers migration and refusal of an unrecognized
  `/etc/sudoers.d/apt-nopasswd` without overwrite or deletion.
- [ ] Verify SSH root operation remains compatible.
- [x] Verify in workflow and architecture tests that release is signaled by
  `Release Tag Signal`, checks tag/SHA ancestry against `origin/main`, and
  publishes only after release-gate.
- [x] Verify the active `Protect release tags v*` ruleset rejects tag update and
  deletion, has no bypass actor, and applies to `refs/tags/v*`.

## Final Gate

- [x] All implementation PRs are merged to `main` at implementation base
  `b8f36e1`; its required CI and CodeQL checks are green.
- [ ] Complete local, Docker, and disposable-host release evidence is recorded.
- [ ] The release-preparation PR is green and merged to `main`.
- [ ] Post-merge `main` CI and CodeQL are green on the final release commit.
- [ ] The `v0.4.8` tag points to that verified final `main` commit.
- [ ] Release Tag Signal, release-gate, GitHub Release, and GHCR publication pass.

No `v0.4.8` tag has been created by this preparation.
