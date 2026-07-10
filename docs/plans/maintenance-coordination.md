# Maintenance Coordination implementation plan

## Outcome

Deepen Maintenance Coordination into one app-scoped module that owns shared admission, exclusive maintenance leases, durable state, public projection, startup recovery, and restored-database handoff. Preserve current backup exclusivity, immediate maintenance responses, scheduled missed-tick behavior, public-state redaction, active Server Action gating, job and audit vocabulary, and restore rollback guarantees.

## Domain decisions

- An exclusive maintenance lease is authoritative; persisted and public maintenance state are projections of that lease.
- Backup export and backup restore are the only exclusive operation classes in the first implementation.
- Backup Operation Lifecycle continues to own active Server Action checks, backup jobs, archive execution, audit facts, and operation outcomes.
- HTTP adapters own path classification and JSON/HTML response rendering; Maintenance Coordination returns transport-neutral admission decisions.
- `/api/maintenance`, maintenance static assets, and Dashboard Event Stream remain admissible during maintenance.
- Scheduled work remains fail-fast and preserves missed-tick and maintenance-skip behavior; it is not queued to execute after maintenance.
- Audit pruning silently skips when shared admission is unavailable.
- Maintenance activation persists before publishing memory. Deactivation persists inactivity before clearing memory.
- Startup clears any stale active marker before unfinished jobs are recovered as interrupted. Startup fails if that reconciliation cannot be persisted.
- Restore handoff reasserts active maintenance state in the newly installed database and again in the original database after rollback.
- Deactivation failure is fail-closed and operator-visible: active state remains visible, ordinary admission stays blocked, and the operation reports a distinct coordination-release failure.
- Admission remains immediate; the first implementation adds no waiting queue, timeout policy, or configurable operation registry.
- Public maintenance state remains limited to `active`, `kind`, `started_at`, and `message`; job ID, actor, lock state, and persistence errors remain internal.
- No database migration or ADR is required.

## Target architecture

### Deep module

Place Maintenance Coordination in `internal/maintenance` and compose exactly one instance through Runtime Composition.

The module interface presents four cohesive capabilities:

1. Initialize and reconcile durable maintenance state during startup.
2. Return the redacted current snapshot used by the status route and maintenance page adapter.
3. Grant or deny shared admission for ordinary requests, scheduled processing, audit pruning, and non-exclusive job-producing work.
4. Grant or deny an exclusive lease for backup export or restore; that lease owns activation, restore handoff, and completion.

Shared and exclusive leases make admission atomic. Callers never receive the underlying mutex or combine a boolean check with a separate lock operation.

The implementation hides:

- reader/writer lock ordering and release;
- persisted and in-memory state coherence;
- the maintenance setting identifier and JSON representation;
- startup stale-marker recovery;
- activation and deactivation ordering;
- fail-closed release behavior;
- restored-database and rollback handoff;
- internal operation facts and public-state redaction.

### Adapters

- A SQLite persistence adapter binds the module to the current application database through the app-scoped database provider.
- An in-memory persistence adapter provides deterministic module and lifecycle tests.
- The HTTP adapter maps paths to work classes, retains bypass routing, and renders the existing JSON or maintenance-page response.
- Backup Operation Lifecycle acquires the exclusive lease, checks active Server Actions, creates the job, activates public state, runs the archive operation, and completes the lease.
- Scheduled Run and policy processing acquire one shared lease for each coordinated execution path and preserve missed-tick facts when denied.
- Audit pruning acquires a short shared lease and silently skips when denied.
- Job Manager stops owning a separate maintenance boolean once every job-producing flow crosses a lease-backed seam.
- Backup archive code retains file preparation, database replacement, runtime reload, session invalidation, and rollback orchestration while invoking the exclusive lease's restore handoff.

## Delivery slices

### Slice 1: Centralize live HTTP and backup admission

Introduce the app-scoped Maintenance Coordination module, SQLite and in-memory persistence adapters, shared and exclusive leases, startup initialization, current snapshot, and public projection.

Migrate the complete live HTTP path: ordinary requests obtain shared admission, backup export and restore obtain exclusive admission before request parsing, Backup Operation Lifecycle activates and completes the exclusive lease after job creation, and the maintenance status/page adapters read the module snapshot.

Preserve active Server Action checks under the exclusive lease, backup response shapes, audit and job facts, `503` JSON/HTML behavior, and the existing status/static/event-stream bypasses.

Remove the process-global maintenance state, the standalone backup barrier, and their direct middleware wiring only after all live route paths cross the module.

Verification:

- module tests for concurrent readers, exclusive denial, reader draining, persist-before-publish activation, public redaction, and deterministic release;
- Backup Operation Lifecycle tests for job-before-activation ordering, active Server Action rejection while exclusive, operation success, and activation failure;
- route contract tests for shared/exclusive admission, JSON and HTML responses, bypass paths, and a second exclusive request;
- Runtime Composition tests proving one module instance reaches middleware, lifecycle, and status adapters;
- an architecture guard preventing restoration of the package-global state, raw barrier wiring, and split initialization/current-state dependencies.

### Slice 2: Move scheduled, audit, and job admission to leases

Migrate non-HTTP consumers to the same module seam. Scheduled policy processing uses one shared lease per coordinated path, remembers missed ticks when denied, and records existing maintenance skips without nested read locks. Audit pruning uses a short shared lease and keeps silent-skip behavior.

Ensure every non-backup job-producing flow is already protected by shared admission, then remove Job Manager's independent maintenance boolean and the remaining `CurrentMaintenanceActive`, `TryBackupRestoreReadLock`, and unlock callbacks from policy, lifecycle, and Runtime Composition wiring.

Verification:

- scheduled tick tests for admitted work, denied work, missed-tick replay, maintenance skip facts, and no nested lease acquisition;
- Scheduled Run tests for shared admission across scan and update execution;
- audit pruning tests for admitted pruning, denial, and admission released after failure;
- job tests proving ordinary jobs cannot begin outside admitted flows and backup jobs remain governed by exclusive lifecycle admission;
- architecture guards preventing raw lock and maintenance-boolean checks outside the module and its adapters.

### Slice 3: Own restore handoff, recovery, and fail-closed completion

Move restored-database maintenance persistence behind the active exclusive lease. Backup archive code invokes restore handoff after installing the restored database and after restoring the original database during rollback; it no longer owns a duplicate maintenance-state type or persistence callbacks.

Make startup reconciliation and completion failure explicit. Startup clears stale active state before unfinished-job interruption. If exclusive completion cannot persist inactivity, preserve active public state, unblock the mutex without admitting ordinary work, record a distinct job and audit failure, and keep the maintenance status endpoint available for diagnosis.

Verification:

- restore tests for active-state handoff to a replacement database, successful runtime reload, session invalidation, and final clear;
- rollback tests proving the original database and active coordination state are restored together;
- startup tests for inactive state, stale active markers, malformed persisted state, and failed stale-state clearing;
- export and restore tests for deactivation persistence failure and operator-visible fail-closed state;
- compatibility tests for public redaction, maintenance responses, job and audit vocabulary, and existing backup archives;
- an architecture guard ensuring raw maintenance-setting SQL and state serialization exist only in the persistence adapter.

## Compatibility constraints

- Keep `/api/maintenance` and its response fields unchanged.
- Keep maintenance HTML, HTTP `503`, and JSON error shapes unchanged except for a distinct internal coordination-release classification.
- Keep `/api/maintenance`, static assets, and Dashboard Event Stream available during maintenance.
- Keep backup export and restore mutually exclusive and blocking for the full request lifecycle.
- Keep active Server Action conflict responses and `active_servers` facts unchanged.
- Keep scheduled missed-tick and maintenance-skip behavior unchanged.
- Keep audit pruning denial silent.
- Keep the `maintenance_state` SQLite row byte-compatible.
- Keep backup archives, restored databases, session invalidation, and rollback behavior compatible with existing releases.
- Do not expose job ID, actor, persistence errors, or lock facts in public maintenance projections.

## Validation gate

Each slice must pass its targeted module, route, Backup Operation Lifecycle, Scheduled Run, audit, job, startup, or restore tests plus:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when live requests, backup operations, scheduled processing, audit pruning, job creation, startup recovery, and restored-database handoff all cross the Maintenance Coordination module; no caller combines maintenance state with a separate lock; compatibility contracts pass; and architecture guards prevent the split barrier-and-boolean design from returning.
