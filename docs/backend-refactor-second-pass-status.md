# Backend Refactor Second Pass Status

This checklist tracks the second backend refactor pass described in [backend-refactor-second-pass-plan.md](backend-refactor-second-pass-plan.md). Phase 0 establishes the safety harness; package extraction starts in Phase 1.

## Phase Status

- [x] Phase 0 - Baseline And Extraction Harness: complete on `codex/backend-second-pass-harness`
- [x] Phase 1 - Events Package: complete on `codex/events-package`
- [x] Phase 2 - Audit Package: complete on `codex/audit-package`
- [x] Phase 3 - App Shell And Config Package: complete on `codex/app-shell-config`
- [x] Phase 4 - Auth Package: complete on `codex/auth-package`
- [x] Phase 5 - Backup Package: complete on `codex/backup-package`
- [x] Phase 6 - Server Inventory Package: complete on `codex/servers-package`
- [x] Phase 7 - Policy Package: complete on `codex/policies-package`
- [x] Phase 8 - Update Package: complete on `codex/updates-package`
- [x] Phase 9 - Observability And Dashboard Package: complete on `codex/observability-dashboard-package`
- [x] Phase 10 - Repository And Schema Ownership: complete on `codex/repository-schema-ownership`
- [x] Phase 11 - Final Global And Wrapper Removal: complete on `codex/final-global-wrapper-removal`
- [ ] Phase 12 - Documentation And Live Smoke

## Phase 0 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `go test -race -count=1 ./...`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 0 because this phase adds documentation and tests only.

## Phase 1 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 1 because this phase only moves the dashboard event broker behind `internal/events`.

## Phase 2 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 2 because this phase only moves audit persistence, listing, pruning, and Markdown rendering behind `internal/audit`.

## Phase 3 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 3 because this phase only moves router/app-shell composition behind `internal/app`.

## Phase 4 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 4 because this phase only moves auth/session behavior behind `internal/auth`.

## Phase 5 Validation

Required:

- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 5 because this phase only moves backup/export restore behavior behind `internal/backup`.

## Phase 6 Validation

Required:

- [x] `go test -count=1 -run 'TestServer|TestHostKey|TestKnownHosts|TestGlobalKey|TestAPIServers|TestServerInventory|TestBackendContract|TestRouteInventory' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 6 because this phase only moves server inventory state, persistence, known_hosts, and SSH auth helper behavior behind `internal/servers`.

## Phase 7 Validation

Required:

- [x] `go test -count=1 -run 'TestPolicy|TestUpdatePolicy|TestScheduled.*Policy|TestDashboard|TestBackendContract|TestRouteInventory' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 7 because this phase only moves scheduled policy persistence, matching, blackout handling, run records, missed-tick replay, and scheduler ownership behind `internal/policies`.

## Phase 8 Validation

Required:

- [x] `go test -count=1 -run 'TestUpdate|TestAutoremove|TestSudoers|TestApproval|TestCVE|TestPostcheck|TestScheduled.*Policy|TestRunnerJobSync|TestMarkdownReport|TestBackendContract|TestRouteInventory' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 8 because this phase only moves update, autoremove, sudoers, approval/cancel, CVE helper, and scheduled scan ownership behind `internal/updates`.

## Phase 9 Validation

Required:

- [x] `go test -count=1 -run 'TestObservability|TestDashboard|TestMetrics|TestBackendContract|TestRouteInventory|TestAppDeps' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 9 because this phase only moves observability summaries, dashboard summaries, metrics rendering, metrics token storage, and metrics summary cache ownership behind `internal/observability`.

## Phase 10 Validation

Required:

- [x] `go test -count=1 -run 'TestSchema|TestRepository|TestServerFacts|TestBackup|TestBackendContract|TestRouteInventory|TestAppDeps' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `go test -race -count=1 ./...`
- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 10 because this phase only moves SQLite schema creation/migration and server-facts repository ownership behind domain packages.

## Phase 11 Validation

Required:

- [x] `go test -count=1 -run 'TestAppDeps|TestBackendContract|TestRouteInventory|TestAuth|TestSession|TestServer|TestBackup|TestAudit|TestPolicy|TestUpdate|TestObservability|TestMetrics|TestJob' ./...`
- [x] `go test -count=1 ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`
- [x] `go test -race -count=1 ./...`
- [x] `go build -o webserver .`
- [x] `npm run test:e2e`

Broader gates:

- [x] `govulncheck ./...`
- [x] `actionlint`
- [x] `npm audit --audit-level=moderate`

Live disposable-host smoke is not required for Phase 11 because this phase only removes compatibility wrappers and app-scopes default runtime dependencies.

## Compatibility Cleanup

- No `//lint:ignore U1000` compatibility wrappers remain.
- Default router dependencies now create fresh app-scoped service, broker, barrier, rate-limiter, server-state, metrics-token, policy, update, backup, audit, and observability instances instead of reusing mutable service singletons.
- Remaining package-level values are process startup state, constants, pure helper functions, compiled regexes, or low-level test hooks that require process-wide replacement until the final command-layout/documentation phase.

## Phase 0 Contract Coverage

- Route inventory remains covered by `criticalRouteInventory`.
- Backend contract tests cover auth/middleware behavior, representative route groups, server list shape, auth/session shape, backup status/export shape, audit/job reports, policy list/create/settings/runs shape, and update approve/cancel route contracts.
