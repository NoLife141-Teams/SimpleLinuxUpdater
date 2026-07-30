# v0.4.2 Release Readiness

This record tracks the pre-tag validation for `v0.4.2`. It is intentionally a
readiness record; the targeted smoke result is recorded separately.

## Candidate

- Prepared: 2026-07-30 America/Toronto
- Release version: `v0.4.2`
- Release implementation commit:
  `472661bb7a8fa913831289bec45a3a2ae7e2228e`
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

## Final Gate

- [x] PR #346 checks are green, including `ci-required`, frontend unit tests,
      npm audit, Playwright, and the Go, JavaScript/TypeScript, and Actions
      CodeQL analyses.
- [x] The post-merge `main` CI and CodeQL runs are green on the release
      implementation commit above.
- [x] The Discord and Telegram notification delta is covered by the targeted
      credential, fan-out, formatting, timeout-isolation, API authentication,
      diagnostics-redaction, and unsafe-input smoke recorded in
      [the v0.4.2 release smoke result](release-v0.4.2-smoke.md).

No `v0.4.2` tag has been created by this preparation.
