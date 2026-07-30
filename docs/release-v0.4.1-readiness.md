# v0.4.1 Release Readiness

This record tracks the pre-tag validation for `v0.4.1`. It is intentionally a
readiness record, not a completed smoke result: the release tag must not be
created until every blocking item below is completed and the final commit SHA
is recorded.

## Candidate

- Prepared: 2026-07-29 America/Toronto
- Release version: `v0.4.1`
- Final release commit: pending
- Release tag: not created

## Local Automated Gate

The following checks passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (no reachable vulnerabilities)
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...`
- `go build -o webserver .`
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:unit` (142 tests)
- `npm run test:e2e` (60 tests)

Version metadata was also checked across `README.md`,
`docs/installation.md`, `docs/deployment.md`, `docs/configuration.md`,
`templates/index.html`, and this changelog.

## Docker Runtime Gate

The release candidate was validated on the configured remote Docker daemon:

- Docker Engine `29.6.2`, Linux `x86_64`
- `docker build --pull --tag simplelinuxupdater:v0.4.1-release-candidate .`:
  pass
- Candidate image:
  `sha256:73c0b6b9f763c54a731c66795bfb7f45562492b702a3b949180ebd2893d68b69`
- Builder image:
  `golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2`
- Runtime image:
  `alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b`
- Fresh named volume initialized the SQLite database with UID/GID `100:101`.
- The webserver process ran as non-root UID `100`.
- `/setup` returned HTTP `200` from both the remote host port and inside the
  container.
- A container restart preserved the volume and `/setup` returned HTTP `200`
  again.

## Blocking Before Tag

- [ ] Record the final release commit SHA above.
- [ ] Confirm CI jobs `unit`, `race`, `cover`, and `ui-e2e` are green on that
      exact commit.
- [ ] Complete the disposable-host workflow in
      [Release Smoke Checklist](release-smoke.md), using a disposable database,
      `known_hosts` file, and Debian/Ubuntu SSH target.
- [ ] Record the live host, scheduled policy, audit/report, backup, and timeout
      guard outcomes in a final `v0.4.1` smoke result.

The disposable-host smoke is currently pending because no disposable target
and release-owner safety confirmation were supplied for this preparation.
That missing external prerequisite must not be interpreted as a skipped or
passing check.
