# Application Time Interpretation implementation plan

## Outcome

Deepen Application Time Interpretation into one app-scoped module that owns configured timezone choice, system detection, effective location resolution, timestamp parsing and display, local wall-clock resolution, DST behavior, persistence coherence, and internal fallback diagnostics. Preserve current timezone settings, HTTP fields, UTC persistence, fixed-offset compatibility, scheduled-policy semantics on ordinary dates, audit vocabulary, and operator-facing timestamp formatting.

## Domain decisions

- The persisted configured choice is authoritative; the effective location and display facts are one immutable runtime interpretation.
- Supported choices are System, IANA named zone, and fixed offset. Legacy `Local` and `Server local time` values are read as System.
- System mode resolves at initialization and when explicitly saved again. Operating-system timezone changes otherwise take effect after restart.
- Explicit invalid persisted configuration fails startup instead of silently scheduling in another timezone.
- System detection may fall back deterministically through a detected IANA zone, usable process-local location, fixed offset, and UTC while recording an internal degraded diagnostic.
- Configuration changes validate and resolve first, persist second, and publish the new interpretation last. Persistence failure leaves the previous interpretation active.
- Persisted jobs, audit records, health facts, and Scheduled Runs remain UTC instants. Existing queued and historical records are never reinterpreted after a timezone change.
- Future unpersisted schedule projections and later scheduler evaluations use the newly accepted interpretation.
- A nonexistent spring-forward wall-clock occurrence is skipped rather than normalized to another time.
- An ambiguous fall-back wall-clock occurrence resolves once to the earlier UTC instant and uses that instant for deduplication.
- Policy occurrence, weekday, no-run-window, and projection decisions use one immutable interpretation snapshot per operation.
- Read-path formatting preserves the current compatibility behavior: empty timestamps render as `-`, valid instants render locally, and unsupported non-empty values remain visible.
- Public application-time responses retain `timezone`, `resolved_timezone`, and `editable_timezone`; internal diagnostics remain private.
- No ADR or database migration is required.

## Target architecture

### Deep module

Place Application Time Interpretation in `internal/apptime` and compose exactly one instance through Runtime Composition.

The module interface presents six cohesive capabilities:

1. Initialize and publish the accepted interpretation from persisted choice and system detection.
2. Return the current immutable interpretation for one complete caller operation.
3. Validate, persist, and publish a new configured choice.
4. Parse persisted absolute timestamps using the existing accepted layouts.
5. Format instants and compatibility values for operator-facing display.
6. Resolve a local date and wall-clock time into a canonical UTC occurrence with explicit valid, nonexistent, or ambiguous semantics.

The implementation hides:

- SQLite setting representation and legacy aliases;
- operating-system metadata and zoneinfo detection;
- IANA and fixed-offset validation;
- process-local and UTC fallback ordering;
- configured, editable, display, and resolved-name coherence;
- immutable publication and concurrent reads;
- persist-before-publish updates;
- timestamp layout compatibility;
- DST gap and overlap detection;
- internal degraded diagnostics.

### Adapters and consumers

- A SQLite choice adapter binds the module to the existing settings row without changing its byte-compatible representation.
- A system-timezone detector adapter owns environment variables, metadata files, localtime links/content matching, process-local fallback, and zoneinfo lookup.
- In-memory choice and detector adapters provide deterministic module tests.
- The application-time HTTP adapter decodes update requests, maps validation and persistence failures, records existing audit facts, and projects the accepted interpretation into the existing response shape.
- Scheduled policy processing and Policy Schedule Projection consume one interpretation snapshot for local-day, weekday, no-run-window, and occurrence decisions.
- Scheduled Run retains cadence, matching, run persistence, jobs, and outcomes while consuming canonical UTC occurrences from the module.
- Audit, Dashboard Projection, Observability, and maintenance-page rendering consume the same interpretation for parsing and display.
- Runtime Composition replaces the separate location, timezone, display-name, and resolved-name callbacks with the single app-scoped module.

## Delivery slices

### Slice 1: Establish accepted interpretation and compatible administration

Introduce the app-scoped module, SQLite and in-memory choice adapters, system detector adapter, immutable interpretation, initialization, configuration update, parsing, formatting, and HTTP projection.

Migrate the application-time status and update endpoints through the module while preserving stored values, legacy aliases, fixed offsets, validation responses, audit facts, and the three public response fields. Initialize the module during startup before consumers and fail startup for invalid explicit configuration or unreadable persistence.

Verification:

- module tests for System, IANA, fixed offset, legacy aliases, detector fallback, explicit invalid configuration, persistence errors, persist-before-publish ordering, immutable concurrent readers, and display compatibility;
- route tests for status, update, validation, persistence failure, audit facts, and unchanged JSON fields;
- startup and Runtime Composition tests proving one initialized module instance;
- compatibility tests for existing setting bytes and restored databases.

### Slice 2: Consolidate timestamp consumers and Runtime Composition

Migrate audit, Dashboard Projection, Observability, maintenance-page display, policy administration responses, and calendar-facing formatting to one accepted interpretation snapshot per operation.

Remove `CurrentAppTimezone`, `CurrentAppLocation`, `AppTimezoneDisplayName`, and `AppTimezoneResolvedName` from Runtime Composition after every consumer crosses the module seam. Retire direct parsing and formatting callbacks outside internal adapters.

Verification:

- audit, dashboard, observability, maintenance-page, policy administration, and calendar contract tests for aligned location and display facts;
- concurrency tests proving an in-flight projection retains its accepted snapshot across a configuration update;
- architecture guards preventing multiplied timezone callbacks, direct settings access, and standalone display parsing outside the module and adapters.

### Slice 3: Own local wall-clock and DST occurrence resolution

Move scheduled local-date and wall-clock interpretation behind the module. Scheduled policy processing, no-run windows, Policy Schedule Projection, calendar output, and Scheduled Run creation consume canonical occurrence results rather than constructing local times directly.

Preserve ordinary-date behavior, stored UTC run identifiers, missed-tick behavior, candidate priority, and queued-run immutability. Add explicit spring-forward skip and fall-back earlier-occurrence semantics, including projection facts sufficient for operators to understand the selected offset.

Verification:

- occurrence tests across ordinary dates, spring-forward gaps, fall-back overlaps, fixed offsets, and multiple IANA zones;
- scheduled tick and calendar tests proving the same interpretation snapshot governs weekday, occurrence, and no-run windows;
- timezone-change tests proving persisted queued/history instants remain unchanged while future projections recalculate;
- deduplication tests proving an overlap creates at most one Scheduled Run;
- architecture guards preventing direct local occurrence construction in scheduled-policy modules.

## Compatibility constraints

- Keep `app_timezone` setting values and restore compatibility unchanged.
- Keep `timezone`, `resolved_timezone`, and `editable_timezone` response fields unchanged.
- Keep IANA, System/local, and fixed-offset choices readable.
- Keep UTC timestamp layouts and stored Scheduled Run identifiers unchanged.
- Keep ordinary-date cadence, matching, priority, blackout, and no-run behavior unchanged.
- Keep audit actions, statuses, messages, and metadata vocabulary unchanged.
- Keep empty and malformed display-value fallbacks unchanged.
- Do not expose detector paths, persistence failures, or degraded diagnostics publicly.
- Do not alter existing queued or historical instants after configuration changes.

## Validation gate

Each slice must pass its targeted module, route, startup, Runtime Composition, audit, projection, observability, policy, calendar, Scheduled Run, compatibility, concurrency, and architecture tests plus:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when all timezone choice, detection, persistence, parsing, formatting, local wall-clock resolution, and DST rules live behind Application Time Interpretation; Runtime Composition owns one instance; consumers use one immutable interpretation per operation; public and persistence contracts remain compatible; and architecture guards prevent separate timezone facts from returning.
