# v0.4.2 Release Readiness

This record tracks the pre-tag validation for `v0.4.2`. It is intentionally a
readiness record, not a completed smoke result: the release tag must not be
created until every blocking item below is completed and the final commit SHA
is recorded.

## Candidate

- Prepared: 2026-07-30 America/Toronto
- Release version: `v0.4.2`
- Final release commit: pending
- Release tag: not created

## Scope

- Add native Discord notification delivery.
- Add native Telegram notification delivery.
- Encrypt destination credentials at rest.
- Keep Discord and Telegram enablement, diagnostics, and test delivery
  independent.
- Format notification payloads for each destination.

## Local Automated Gate

The following checks passed on the release-preparation worktree:

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...` (no reachable vulnerabilities)
- `actionlint`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -covermode=atomic -coverprofile=coverage.out ./...` (72.6% total)
- `go build -o webserver .`
- `npm audit --audit-level=moderate` (zero vulnerabilities)
- `npm run test:unit` (143 tests)
- `npm run test:e2e` (60 tests)

Version metadata was also checked across `README.md`,
`docs/installation.md`, `docs/deployment.md`, `docs/configuration.md`,
`templates/index.html`, and this changelog.

## Docker Runtime Gate

The release candidate was validated on the configured remote Docker daemon:

- Docker Engine `29.6.2`, Linux `x86_64`
- `docker build --pull --tag simplelinuxupdater:v0.4.2-release-candidate .`:
  pass
- Candidate image:
  `sha256:fe320da793b7d224fb3e916f0a040b5bf50aadfed87c4ea251ba639c262f50e2`
- Builder image:
  `golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2`
- Runtime image:
  `alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b`
- A fresh anonymous volume initialized the SQLite database with UID/GID
  `100:101`.
- The `webserver` process ran as non-root UID/GID `100:101`.
- `/setup` returned HTTP `200` before and after a container restart using the
  same volume.
- The temporary container and anonymous volume were removed after the gate.

## Blocking Before Tag

- [ ] Record the final release commit SHA above.
- [ ] Confirm CI jobs are green on that exact commit.
- [ ] Complete the release smoke appropriate to the Discord and Telegram
      notification delta without recording webhook URLs, bot tokens, chat IDs,
      or message contents that contain secrets.

No `v0.4.2` tag has been created by this preparation.
