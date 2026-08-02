# v0.4.4 Release Readiness

This record tracks the pre-tag validation for `v0.4.4`. The disposable-host
results are recorded separately in the release smoke result.

## Candidate

- Prepared: 2026-08-02 America/Toronto
- Release version: `v0.4.4`
- Release implementation commit: `f2b9990`
- Release preparation branch: `codex/release-v0.4.4`
- Release tag: not created

## Scope

- Persist uncertain APT outcomes as `needs_reconciliation`, block additional
  package mutations, and provide confirmed APT inspection and repair.
- Add Recommended action guidance and a controlled, verified reboot workflow.
- Add a plan-aware disk-space gate and an explicit non-interactive APT/dpkg
  strategy.
- Add deterministic canary-and-wave policy previews and success-gated rollout.
- Extend a lock-confirmed APT mutation instead of replaying it after a timeout.
- Align the active documentation with the current maintenance workflows and
  immediate Status actions.

## Local Automated Gate

The complete release gate passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (no reachable vulnerabilities; one module advisory is in
  code that is not called)
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (73.1% total)
- `go build -o webserver .`
- `npm ci`
- `npm run test:unit` (148 tests)
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:e2e` (60 tests)

Version metadata was checked across `README.md`, `docs/installation.md`,
`docs/deployment.md`, `docs/configuration.md`, `templates/index.html`, and
`CHANGELOG.md`.

## Docker Runtime Gate

The release candidate was validated on the configured remote Docker daemon:

- Docker Engine `29.6.2`, Linux `amd64`
- Candidate image:
  `sha256:d8ad53263ac0d373e4bf802ee8b44cf80e259280adcbb4c493116a9cb41e05c7`
- Builder image:
  `golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2`
- Runtime image:
  `alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b`
- A fresh named volume initialized the SQLite database with UID/GID `100:101`
  and file mode `0600`.
- The `webserver` process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.

## Disposable-Host Gate

The live candidate passed the update, backup, scheduling, audit, and
observability checks recorded in
[the v0.4.4 release smoke result](release-v0.4.4-smoke.md):

- 13 approved Ubuntu security updates installed successfully.
- Baseline and plan-aware disk checks passed, and the explicit non-interactive
  APT strategy appeared before mutation.
- The final no-op scan reported Healthy with APT health passing and no reboot
  required.
- Encrypted backup export and read-only restore verification passed with
  `known_hosts` included.
- A disabled one-host canary/wave policy previewed correctly, then two scan-only
  scheduled occurrences completed successfully before the policy was disabled
  again.
- Observability showed the disposable target, two update runs, health trends,
  and all 24-hour, 7-day, and 30-day windows.
- Audit history contained update, backup, and scheduled-run events with report
  links.

## Final Gate

- [ ] Release-preparation PR checks are green, including `ci-required`,
      frontend unit tests, npm audit, Playwright, Go quality gates, and CodeQL.
- [ ] The release-preparation PR is merged to `main`.
- [ ] Post-merge `main` CI and CodeQL are green on the final release commit.
- [ ] The `v0.4.4` tag points to that verified final release commit.

No `v0.4.4` tag has been created by this preparation.
