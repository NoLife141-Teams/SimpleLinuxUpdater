[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Architecture

SimpleLinuxUpdater is a single Go binary with a Gin web server, server-rendered pages, JSON APIs, SQLite persistence, and SSH runners for Debian/Ubuntu maintenance. The backend now uses internal packages for domain ownership and an app-scoped runtime composed through `AppDeps`; `package main` remains responsible for process startup, DB opening, route adapters, and command-level wiring.

## Table of contents

- [Runtime shape](#runtime-shape)
- [Request flow](#request-flow)
- [Services and state](#services-and-state)
- [Data storage](#data-storage)
- [Host maintenance sessions](#host-maintenance-sessions)
- [Update runner lifecycle](#update-runner-lifecycle)
- [Scheduled policies](#scheduled-policies)
- [Audit, reports, and observability](#audit-reports-and-observability)
- [Development shape](#development-shape)

## Runtime shape

- Web server: Go + Gin, HTML templates under `templates/`, static assets under `static/`.
- Route registry: `setupRouterWithDeps(AppDeps)` builds the engine, middleware, sessions, jobs, templates, static files, and then calls `registerRoutes`.
- Dependency boundary: `AppDeps` provides injectable DB, service, job-manager, session, timezone, dashboard-event, Maintenance Coordination, server-state, and initialization dependencies.
- Services: audit, auth, backup, events, jobs, observability, policy scheduling, server inventory, and update runner behavior live behind `internal/...` package boundaries.
- Runtime state: default router setup creates fresh app-scoped services, broker, Maintenance Coordinator, rate limiters, job manager, server state, session manager, metrics-token service, policy service, update service, backup service, audit service, and observability service.
- Schema ownership: each domain package owns its SQLite table creation/migration; `package main` calls those installers in a deterministic startup order.
- UI: Status, Manage, Observability, and Admin pages are backed by JSON APIs and live dashboard events. Observability Page Interaction owns accepted summary and health-trend snapshots, independent freshness, host selection, stale-response rejection, and completion-aware refresh decisions while its browser adapter retains fetch, timer, visibility, timezone, and DOM effects.

## Request flow

1. `setupRouter()` delegates to `setupRouterWithDeps(NewDefaultAppDeps())`.
2. Router setup configures trusted proxies and security headers, reconciles Maintenance Coordination before job recovery, then initializes sessions, templates, and static files.
3. `registerRoutes(router, deps)` registers public setup/login/status routes first.
4. Protected routes are installed after `authGateMiddleware()` and `sameOriginWriteMiddleware()`.
5. Route groups pass `AppDeps` into server/action routes, policy/audit/report routes, dashboard summaries, metrics, backup, auth/session, and dashboard events.
6. Handlers preserve HTTP paths, JSON shapes, middleware behavior, and status codes while delegating business behavior into package-owned services.

## Services and state

- `internal/audit.Service` writes audit rows, lists audit events, prunes old rows, and renders Markdown reports.
- `internal/servers.Service` owns server CRUD, tag normalization, secret persistence, rollback behavior, and per-server known-host operations.
- `internal/updates.Service` owns update, autoremove, sudoers, approval, scheduled-scan, job, and audit runner behavior; it consumes Host Maintenance Session for authenticated host execution.
- `internal/policies.Service` owns scheduled-policy validation, matching, blackout handling, due-slot processing, missed-tick replay, scheduler ticks, and interrupted-run recovery.
- `internal/observability.Service` owns dashboard/observability summaries, metrics rendering, metrics token persistence, and metrics cache behavior.
- `internal/jobs.Manager` owns persisted job creation, update, recovery, structured job-log fragments, bounded compatibility previews, log retention, runtime-status sync callbacks, and dashboard notifications after successful writes.
- `internal/maintenance.Coordinator` owns shared admission, exclusive backup leases, durable/public maintenance state, startup recovery, and restore handoff.
- `internal/apptime.Module` owns the accepted application timezone interpretation, compatible timestamp display, and canonical local wall-clock resolution including DST gaps and overlaps.
- `internal/backup` owns archive validation, file replacement, rollback, runtime reload, and session invalidation; it invokes the active exclusive lease at restore handoff boundaries.
- `internal/events.Broker` owns dashboard event fan-out for SSE clients.
- `internal/servers.State` owns the server inventory snapshot and live status map for each app instance. Process-wide globals remain only for process startup, constants, pure helpers, and low-level test seams that must be replaced at process scope.

## Data storage

SQLite table ownership:

- `internal/servers`: `servers`.
- `internal/auth`: `auth_users`, `sessions`, and `sessions_expiry_idx`.
- `internal/audit`: `audit_events` and audit indexes.
- `internal/health`: `server_facts`, `server_health_snapshots`, and their indexes.
- `internal/jobs`: `jobs`, `job_log_chunks`, and job/log indexes.
- `internal/policies`: `update_policies`, `update_policy_overrides`, `update_policy_runs`, and policy-run indexes.
- Shared main/app schema: `settings`, used by maintenance state, global SSH key storage, policy settings, app timezone/blackout settings, and metrics token state.

SQLite stores server inventory, encrypted credentials, audit events, auth/session state, persisted jobs, scheduled policy state, current Server facts, time-ordered health snapshots, metrics token state, backup/restore metadata, and related operational state.

An encryption key is stored in `config.json` alongside the DB, typically under `/data`.

Legacy import:

- On first run, the app may import `servers.json` if present, then uses SQLite going forward.

## Host maintenance sessions

Host Maintenance Session is the bounded execution context used by Update Service for one server maintenance phase. Its production adapter owns SSH authentication setup, host-key verification, dialing, command timeouts, reconnect and connection replacement, retry events and accounting, semantic host inspection, and idempotent transport closure.

Update Service retains Server Action Lifecycle, Approval Scope, Package Discovery and Upgrade Plan interpretation, Scheduled Run behavior, jobs, audit, runtime status, and persistence. It receives structured session outcomes and never receives an SSH client configuration, raw connection, or mutable connection pointer.

One runner owns one session at a time. CVE enrichment opens an independent read-only session. A manual approval wait keeps the Server Action reserved but closes the discovery session; approval opens a fresh session before the approved upgrade, while cancellation or approval timeout does not reconnect.

## Update runner lifecycle

Typical update:

1. A route creates a persisted job and starts the runner through `UpdateService`.
2. Host Maintenance Session builds SSH authentication and host-key verification from per-server credentials, global key fallback, and known-hosts configuration, then opens the bounded discovery session.
3. Pre-checks run before `apt-get update`.
4. `apt-get update` runs with retry and timeout metadata.
5. Simulated upgrade determines pending packages.
6. Status becomes `pending_approval` when approval is required; the discovery session closes while the Server Action remains reserved.
7. Approval opens a fresh session, while cancellation or timeout finishes without reconnecting.
8. Upgrade runs with all packages or scoped security packages through the active session.
9. Post-update health checks run when enabled.
10. Job state, status map, audit metadata, server facts, and dashboard events are updated.

Autoremove, sudoers enable/disable, CVE enrichment, and scheduled scans use the same job/status/report foundations.

## Scheduled policies

Scheduled update policies support legacy `target_tag`, `include_tags`, `exclude_tags`, explicit `target_servers`, per-server overrides, global blackouts, and per-policy blackout windows. `PolicyService` evaluates matching, due slots through Application Time Interpretation, skip reasons, missed scheduler ticks during backup restore, and run creation before handing execution to the update service. Nonexistent spring-forward occurrences are unavailable; ambiguous fall-back occurrences canonicalize once to the earlier UTC instant.

Policy route adapters still live in `package main` and keep the existing wire format. Matching, validation, persistence, skipped-run recording, scheduler ticks, and missed-tick replay live in `internal/policies`.

## Audit, reports, and observability

Actions record actor, client IP, action, target type/name, status, message, and sanitized metadata. Raw audit writes do not notify dashboard clients; service-backed record calls notify after a successful write.

Markdown reports are generated for:

- audit events at `/api/reports/audit/:id`;
- persisted jobs at `/api/reports/jobs/:id`.

Detailed job output is read from ordered `job_log_chunks`; reports surface expiration and truncation explicitly. Authenticated clients can page fragments with `GET /api/jobs/:id/logs?after_seq=N&limit=N`. The legacy `jobs.logs_text` field remains a bounded compatibility preview.

The observability dashboard and `/metrics` endpoint derive summaries from `update.complete` audit events and related persisted runtime data:

- totals and success rate;
- average duration when duration metadata exists;
- failure-cause aggregation;
- policy/run/job summaries used by dashboard panels.

Dashboard event streaming uses the app-scoped client event broker. The UI can fall back to polling when live events are unavailable.

## Development shape

- `setupRouter()` remains the production entrypoint and delegates to `setupRouterWithDeps(NewDefaultAppDeps())`.
- `setupRouterWithDeps` is the test and composition seam for injecting services, state, DB providers, session managers, job managers, event brokers, rate limiters, and time providers.
- `newIsolatedTestApp(t)` creates a temp DB, temp `known_hosts`, fresh app-scoped runtime dependencies, and authenticated-session helpers for HTTP contract tests.
- New domain behavior should go into the owning internal package first, with route adapters limited to request parsing, auth/CSRF placement, response shape preservation, audit calls, and dependency wiring.
