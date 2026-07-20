# v0.3.0 Release Smoke Result

This result records the pre-tag validation performed for `v0.3.0`. Credentials, private keys, session cookies, and backup passphrases are intentionally omitted.

## Scope

- Date: 2026-07-20 UTC
- Release branch: `release/v0.3.0`
- Base app commit: `8cb3e3a73ef1a25cbb1d00012f38a4507884c312`
- Disposable app database: `/tmp/slu-release-v030.eVcFcU/servers.db`
- Disposable app `known_hosts`: `/tmp/slu-release-v030.eVcFcU/known_hosts`
- Disposable target name: `release-smoke-target`
- Target OS: Ubuntu 25.04, kernel `6.14.0-37-generic`

## Live Host Result

- Non-destructive SSH reachability and `apt-get -s upgrade` passed.
- The target reported zero pending upgrades before application actions began.
- Host-key scan returned the fingerprint confirmed for the disposable VM; trust persisted one entry in the disposable `known_hosts` file.
- Host-fact refresh reported healthy APT and disk state, matching running/latest-installed kernels, and no reboot requirement.
- The update action completed with status `done` and no pending packages.
- A concurrent duplicate update was rejected with HTTP `409` while the first action was active.
- A disabled explicit-target `scan_only` policy created no run.
- After enabling it for the next UTC slot, the scheduler created one run for only `release-smoke-target`; it completed with status `succeeded` and summary `Scheduled scan completed: no pending updates`.
- Audit and job Markdown reports returned HTTP `200`.
- The 7-day observability summary reported one successful update and a 100% success rate; host-health trends contained five samples for the target.
- Dashboard Projection reported one done host and zero in-progress or pending-approval hosts after completion.

## Backup Result

- Encrypted backup export returned HTTP `200` and included the disposable `known_hosts` file.
- Non-mutating backup verification returned HTTP `200` with valid manifest, configuration, database, and archive results.
- Restore was not executed because export plus non-mutating verification covered the release requirement without introducing unnecessary destructive state replacement.

## Automated Gate

- `go vet ./...`: pass
- `staticcheck ./...` with `v0.7.0`: pass
- `govulncheck ./...` with `v1.3.0`: pass; no reachable vulnerability found
- `actionlint` with `v1.7.12`: pass
- `go test -count=1 ./...`: pass
- `go test -race -count=1 ./...`: pass
- `go test -covermode=atomic -coverprofile=coverage.out ./...`: pass; total statement coverage 70.3%
- `go build -o webserver .`: pass
- `npm ci`: pass
- `npm audit --audit-level=moderate`: pass; zero vulnerabilities
- `npm run test:unit`: pass; 75 tests
- `npm run test:e2e`: pass; 18 tests
- Live headless UI check: Status, Manage, Admin, and Observability pass; version `v0.3.0`, trusted-host state, scheduled policy, host trends, and desktop/mobile overflow checks pass.

## Docker Result

- Builder aligned with `go.mod`: `golang:1.26.5-alpine`.
- Official builder image resolved to digest `sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2` during validation.
- `docker build --pull --tag simplelinuxupdater:v0.3.0-release-candidate .`: pass.
- The candidate container started successfully and returned HTTP `200` from `/setup` on an isolated port.

## Intentionally Skipped

- Package approval was unnecessary because the disposable target had no pending packages.
- Autoremove and sudoers mutation were not run because the clean target provided no release-specific reason to change them.
- Backup restore, key removal, password removal, audit pruning, metrics-token rotation, and external notification delivery were not executed. Their deterministic automated coverage passed, and destructive or external effects were outside this smoke's necessary scope.
- The one-second SSH timeout guard was not forced against the healthy live target; timeout and shutdown behavior passed the automated Go test suites, including the race detector.
