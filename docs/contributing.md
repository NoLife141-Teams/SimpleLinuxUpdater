[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Contributing

## Table of contents

- [Who we need](#who-we-need)
- [How to help](#how-to-help)
- [If you want to code](#if-you-want-to-code)
- [Backend conventions](#backend-conventions)
- [Testing pattern](#testing-pattern)
- [Implementation examples](#implementation-examples)
- [Build and test](#build-and-test)

## Who we need

If you run Linux servers (Debian/Ubuntu) or a homelab, your feedback is the most valuable input for this project. The goal is to make updates safer and more predictable in real environments.

Examples of useful contributors:

- Homelab users running a few machines and willing to test upgrades.
- Linux admins managing multiple hosts and able to share operational expectations.
- Anyone who can provide clear bug reports and reproduction steps.

## How to help

You do not need to write code to contribute.

High-impact contributions:

- File bug reports with:
  - your OS, for both updater host and target host;
  - what you clicked or what command you ran;
  - relevant server logs from the UI.
- Suggest features or defaults, such as health-check blocking policies, retry behavior, or approval workflow.
- Share what you want to see in observability, especially which metrics and failure causes are useful.
- Improve docs for clarity, missing steps, or safer deployment guidance.

## If you want to code

If you want to implement a fix or feature:

- Open an issue first, or comment on an existing one, describing the change.
- Keep changes focused and add tests when behavior changes.
- Preserve route paths, JSON fields, schemas, middleware behavior, audit event names, job strings, template paths, and static asset paths unless the change explicitly requires a migration.
- Submit changes via a pull request.

## Backend conventions

- `setupRouter()` is the production entrypoint and delegates to `setupRouterWithDeps(NewDefaultAppDeps())`.
- `setupRouterWithDeps(AppDeps)` composes runtime dependencies and delegates Gin engine creation, trusted proxies, middleware, maintenance/job/session initialization, templates, static files, and route registration to `internal/app.NewRouter`.
- `registerRoutes(router, deps)` is the route wiring entrypoint. Public routes must stay before `authGateMiddleware`; protected write routes must stay behind `sameOriginWriteMiddleware`.
- Use the owning internal package when adding domain behavior:
  - `internal/audit` for audit persistence, listing, pruning, and Markdown reports.
  - `internal/app` for transport-level Gin router construction and trusted-proxy parsing.
  - `internal/auth` for users, sessions, same-origin helpers, and rate limiters.
  - `internal/backup` for backup archive/export/restore behavior and restore barriers.
  - `internal/apptime` for configured/system timezone interpretation and civil-time resolution.
  - `internal/health` for current Server facts, Health Snapshot Capture, history, retention, rename, and deletion continuity.
  - `internal/maintenance` for app-wide ordinary-work admission and exclusive maintenance leases.
  - `internal/notifications` for notification admission, redaction, delivery, retry, recorded outcome, and shutdown.
  - `internal/runtime` for runtime status projection and job/status reconciliation facts.
  - `internal/servers` for inventory state, persistence, credentials, known_hosts, and SSH auth helpers.
  - `internal/policies` for scheduled policy validation, matching, run records, scheduler ticks, and missed-run replay.
  - `internal/scheduledruns` for Scheduled Run admission, scan/update job creation, skipped outcomes, job reconciliation, and terminal audit publication.
  - `internal/updates` for update/autoremove/sudoers execution, approval/cancel, CVE enrichment, SSH retries, and scheduled scans.
  - `internal/observability` for dashboard summaries, observability summaries, Prometheus rendering, and the Metrics Access Credential lifecycle.
  - `internal/events` and `internal/jobs` for dashboard SSE fan-out and persisted job management.
- Use `AppDeps` to inject DB providers, services, runtime state, job managers, session managers, event brokers, rate limiters, and time providers. Do not add mutable package-level service singletons.
- Do not put new business logic directly in route registration. Route handlers should parse requests, preserve response behavior, and delegate.

## Testing pattern

Use service tests when the behavior can be tested without Gin. Use HTTP tests when the important behavior is routing, auth, CSRF, status codes, or JSON shape.

Preferred app fixtures:

- `newIsolatedTestApp(t)` for a fully isolated router with temp DB, temp `known_hosts`, fresh session state, fresh job manager, reset rate limiters, reset metrics token state, and empty in-memory server/status state.
- `newTestAppWithDeps(t, dbFile, deps)` when a route must use injected services or callbacks.
- `app.authenticate(t)` to create the admin account and return an authenticated session cookie.

Fixture rules:

- Prefer `newIsolatedTestApp(t)` for HTTP contract coverage so tests do not share DB rows, sessions, jobs, auth rate limits, server state, backup barriers, dashboard brokers, metrics-token caches, SSH/known_hosts hooks, or maintenance state.
- Prefer package-level service/repository tests for validation, matching, persistence, parsing, retry, and report rendering logic that does not require Gin middleware.
- Use `newTestAppWithDeps` only when the behavior under test needs an injected service, fake callback, fixed time provider, custom job manager, or custom DB provider.
- Keep route tests focused on auth, same-origin behavior, status codes, JSON keys, headers, downloads, and route inventory.

Example route test shape:

```go
app := newIsolatedTestApp(t)
cookie := app.authenticate(t)

req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
req.AddCookie(cookie)
rec := httptest.NewRecorder()
app.Handler.ServeHTTP(rec, req)
```

Example injected service shape:

```go
app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "app.db"), AppDeps{
    PolicyService: NewPolicyService(PolicyServiceDeps{
        ListPolicies: func() ([]UpdatePolicy, error) {
            return []UpdatePolicy{policy}, nil
        },
        SnapshotServers: func() []Server {
            return []Server{{Name: "srv-prod", Tags: []string{"prod"}}}
        },
    }),
})
```

## Implementation examples

### Add a protected API route

1. Add the route in the smallest matching route group in `registerRoutes`.
2. If the handler needs a dependency, pass `AppDeps` into the group and call a `handle...WithDeps` helper.
3. Keep the existing middleware order: public routes first, then `authGateMiddleware`, then `sameOriginWriteMiddleware`.
4. Put behavior in the owning internal service or repository unless the route only composes existing behavior.
5. Add the critical method/path pair to the route inventory test when the endpoint is user-facing or relied on by the UI.
6. Add an HTTP test with `newIsolatedTestApp` or `newTestAppWithDeps` for auth, status code, and JSON shape.

### Add a new audit event

1. Use the app's injected audit service from route dependencies, or the `internal/audit.Service` directly in package-level tests.
2. Use stable action names such as `feature.action` and keep metadata JSON small, sanitized, and free of secrets.
3. Record failures and successes close to the operation that actually succeeded or failed.
4. Do not change `AuditEvent` JSON fields, report routes, or existing report formatting unless the change includes compatibility tests.
5. Add service or route tests that assert the action, status, target, and important metadata keys.

### Add a scheduled-policy rule

1. Add matching or validation behavior in `internal/policies.Service`, not directly in route handlers.
2. Preserve existing targeting inputs: `target_tag`, `include_tags`, `exclude_tags`, `target_servers`, and per-server overrides.
3. Preserve run-record behavior: skipped, superseded, blackout, maintenance, missing-server, no-match, and busy reasons should remain explicit.
4. Keep due-slot calculations in the app timezone.
5. Add table-driven service tests for matching/validation and at least one route or scheduler test when the API response changes.

## Build and test

Build:

```bash
go build -o webserver .
```

Run tests:

```bash
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
npm ci
npm run test:unit
npm run test:e2e
```

Coverage:

```bash
go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -n 1
```

The `test (cover)` CI matrix entry enforces the versioned global statement-coverage floor in `.github/workflows/ci.yml`. The initial floor is `73.0%`, based on a `73.3%` measurement from `main`; CI prints both the measured value and required minimum. A coverage failure makes the `test` job fail and therefore also fails `ci-required`. The trusted release gate enforces the same floor.

To raise the floor intentionally, first synchronize `main`, run the exact coverage commands above, then update `GO_COVERAGE_THRESHOLD` to the same higher value in both `ci.yml` and `release.yml` and update `ci_coverage_architecture_test.go`. Run the Go tests and `actionlint` before opening the PR. Do not lower the floor merely to make a PR pass; add tests for the regressed paths instead. Any exceptional reduction requires an explicit technical justification and review in its own change.

Optional hardening checks when tools are available:

```bash
staticcheck ./...
govulncheck ./...
actionlint
npm audit --audit-level=moderate
```

## Go toolchain

The exact `go` directive in `go.mod` is the canonical Go toolchain version. GitHub Actions reads that file through `setup-go`, and the Docker builder must use the matching `golang:<version>-alpine` tag. Run `tools/ci/verify-go-toolchain.sh` after either file changes. CI requires this check on every PR, and Dependabot is configured not to update the `golang` Docker image independently; intentional Go upgrades must update `go.mod`, the Docker builder, and the current-version documentation together.

## Release tags

Create release tags only after the release commit is merged to `main` and its final CI and CodeQL runs are green. `.github/workflows/release-trigger.yml`, which is loaded from the tagged ref, has no permissions and only signals `.github/workflows/release.yml`. GitHub loads the latter through `workflow_run` from the trusted default branch. It verifies the exact tag-to-SHA binding and rejects a tagged commit that is not in current `origin/main` history before either publication job can start. Its critical validation set must remain aligned with normal CI; `release_workflow_architecture_test.go` enforces that contract.

Release workflow actions must use full commit SHAs with a readable version comment. GitHub's hosted `Protect release tags v*` ruleset separately prevents updates and deletions of existing `v*` tags. Because repository rulesets are not versioned with this checkout, verify the active ruleset in repository settings as part of release preparation.
