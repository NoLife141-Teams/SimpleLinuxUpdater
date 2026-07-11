# Health Snapshot Capture implementation plan

> Superseded by the accepted Host Health Observation deepening. The shipped implementation owns current Server facts and time-ordered health snapshots in `internal/health`; see `CONTEXT.md`, `docs/architecture.md`, and `health_snapshot_capture_architecture_test.go`. The remainder of this document is retained as historical planning context and is not current implementation guidance.

## Outcome

Deepen `internal/updates` so Health Snapshot Capture owns the accepted recording of time-ordered Server health observations from collected facts and completed maintenance. Replace the audit-to-observability shaping leak with transport-neutral capture inputs while preserving current snapshot rows, retention, failure policies, Dashboard Projection behavior, and operator-visible results.

## Domain decisions

- Health Snapshot Capture is the canonical owner of accepted Server health history, including normalization, persistence, and retention.
- The module accepts two domain inputs: collected Server facts and completed maintenance facts. Callers do not construct persistence records.
- Completed maintenance uses transport-neutral kinds such as completed update and completed Scheduled Run; audit action strings remain an audit-adapter concern.
- Unrelated audit events are rejected by the adapter before they reach Health Snapshot Capture.
- Update package counts preserve the current precedence: pending count, then approved count. Security counts preserve direct security metadata, then security-only approved count, then discovery metadata fallback.
- Precheck results are interpreted before postcheck results so a later postcheck for the same health dimension remains authoritative.
- Missing or malformed completion metadata produces zero counts and unknown health where the current behavior does; the raw metadata is retained and defaults to `{}` when absent.
- A reboot check command error remains unknown rather than being interpreted as requiring a reboot.
- Captures are append-only. This change does not add deduplication, idempotency keys, or a schema migration.
- Collected-facts capture errors continue to propagate to the caller after current facts are saved.
- Completion capture remains best-effort after an audit event is accepted; failures are logged and do not reject the accepted audit record.
- Dashboard Projection remains read-only. It may consume shared pure health-result interpretation, but it does not own or invoke history capture.
- `internal/updates` must not depend on audit or observability packages. Input adapters translate those modules' facts into the capture interface.
- No event bus, new package, UI change, endpoint change, or ADR is required.

## Target architecture

### Deep module

Deepen `internal/updates` with a small Health Snapshot Capture interface:

- `CaptureFacts(ServerFactsRecord) error` accepts collected Server facts;
- `CaptureCompletion(MaintenanceCompletion) error` accepts completed maintenance facts.

The implementation hides:

- conversion from either input into the persisted health snapshot shape;
- count precedence and metadata fallback rules for completed maintenance;
- disk, APT, and reboot health interpretation;
- normalization of Server name, capture time, source, status values, and raw metadata;
- SQLite insertion and per-Server retention pruning.

`MaintenanceCompletion` carries only domain facts needed by capture: Server identity, completion time, maintenance kind, outcome, and accepted metadata. It does not expose audit action strings or observability projection types.

Pure interpretation of health-check results belongs beside the capture model in `internal/updates`, where `PrecheckResult` already lives. Dashboard Projection can reuse that interpretation while retaining ownership of freshness, presentation, and current-state projection.

### Adapters

- The Server facts repository invokes `CaptureFacts` after saving the current facts record and preserves its existing error propagation.
- The audit callback filters supported Server completion events, translates action and metadata into `MaintenanceCompletion`, and invokes `CaptureCompletion` with best-effort logging.
- Update completion maps to last-update outcome; Scheduled Run completion or failure maps to last-scan outcome.
- SQLite remains the persistence adapter behind the capture module and keeps the existing `server_health_snapshots` schema and retention behavior.
- Dashboard Projection consumes the shared pure health-result interpretation without gaining write responsibilities.

## Delivery slices

### Slice 1: Establish capture through collected Server facts

Introduce the Health Snapshot Capture interface and SQLite implementation, then route the existing collected-facts path through `CaptureFacts`. Move record construction, normalization, insertion, and retention behind the seam without changing stored values or the caller's failure behavior.

Verification:

- module tests for exact facts-to-snapshot mapping, defaults, validation, capture time, source, and raw facts retention;
- SQLite tests for insertion, ordering, retention pruning, Server rename, and Server deletion behavior;
- repository integration tests proving facts are saved before capture and capture failures still propagate.

### Slice 2: Capture completed update observations

Add `MaintenanceCompletion` and `CaptureCompletion`, then adapt accepted `update.complete` audit events into completed-update facts. Preserve package-count precedence, security-count fallbacks, outcome mapping, precheck/postcheck precedence, malformed metadata handling, raw metadata retention, and best-effort error logging.

Verification:

- module tests for update outcome, count precedence, security-only approval, discovery fallback, disk/APT/reboot interpretation, postcheck precedence, and malformed or absent metadata;
- audit adapter tests proving unrelated or non-Server events are ignored and supported completion facts are translated once;
- integration tests proving an accepted audit record is not rejected when capture fails.

### Slice 3: Capture completed Scheduled Run observations

Route accepted `schedule.run.completed` and `schedule.run.failed` audit events through the same completion seam. Map discovery and error metadata into last-scan observations while preserving current counts, health interpretation, source, raw metadata, and best-effort failure policy.

Verification:

- module tests for successful and failed Scheduled Run outcomes, discovery count fallbacks, health results, missing metadata, and raw metadata;
- audit adapter tests for both supported Scheduled Run actions and ignored actions;
- end-to-end lifecycle tests proving scheduled completion and failure each create the same history facts as before.

### Slice 4: Share health-result interpretation and close the seam

Make Dashboard Projection consume the pure health-result interpretation owned beside Health Snapshot Capture, remove obsolete audit snapshot construction and observability shaping dependencies, and add architecture guards that keep capture rules and writes inside `internal/updates`.

Verification:

- compatibility tests proving Dashboard Projection freshness and displayed health facts are unchanged;
- focused regression tests for disk capacity, APT status, reboot-required, command-error, and precheck/postcheck behavior across capture and projection;
- architecture tests preventing audit callbacks from constructing persistence records and preventing Dashboard Projection from writing health history;
- full repository validation after obsolete helpers and callback wiring are removed.

## Compatibility constraints

- Keep the `server_health_snapshots` schema, columns, ordering, indexes, and retention limit unchanged.
- Keep current snapshot values, sources, timestamps, count precedence, outcome strings, health statuses, and raw JSON behavior unchanged.
- Keep current facts persistence, history listing, Server rename, and Server deletion behavior unchanged.
- Keep accepted audit action names, target types, metadata fields, recording order, and audit acceptance behavior unchanged.
- Keep update and Scheduled Run lifecycle behavior, jobs, notifications, and operator-visible results unchanged.
- Keep Dashboard Projection freshness, current health facts, labels, endpoint payloads, and template rendering unchanged.
- Do not add deduplication, an event bus, a new package, a schema migration, a background worker, or new product behavior.
- Do not refactor unrelated approval, maintenance, scheduling, audit persistence, or frontend modules.

## Validation gate

Each slice must pass its focused module, adapter, SQLite, audit, lifecycle, and projection tests. The final implementation must also pass:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when callers submit collected facts or transport-neutral completed maintenance facts; `internal/updates` alone owns snapshot construction, normalization, persistence, and retention; audit no longer depends on observability shaping or constructs persistence records; Dashboard Projection remains read-only while sharing pure interpretation; existing records and failure policies remain compatible; and architecture guards preserve the seam.
