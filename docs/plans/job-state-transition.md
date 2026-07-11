# Job State Transition implementation plan

## Outcome

Deepen the existing jobs module around Job State Transition so lifecycle callers express semantic intent while the module owns legal movement, kind-specific phase rules, timestamps, optimistic concurrency, persistence, Runtime Status Projection publication, dashboard notification, and restart interruption. Preserve existing job kinds, statuses, phases, identifiers, reports, audit vocabulary, scheduled metadata, parent-child relationships, and historical records.

## Domain decisions

- Job State Transition is the canonical owner of accepted job lifecycle mutations.
- The common status machine is `queued -> running`, `running -> waiting_approval`, `waiting_approval -> running`, and any nonterminal status to an allowed terminal outcome.
- `succeeded`, `failed`, `cancelled`, and `interrupted` are terminal and immutable.
- Repeating the already accepted terminal outcome is idempotent and does not rewrite terminal facts.
- Job-kind phase policy is internal to the module. New transitions are strictly validated while historical and restored records remain readable.
- Entering `running` for the first time assigns `started_at`. Returning from approval wait preserves it.
- Every accepted terminal transition selects the complete phase and assigns `finished_at` from the injected clock.
- Callers never supply lifecycle timestamps.
- Every mutable job carries an internal monotonically increasing revision. Transitions use atomic compare-and-set and increment it on acceptance.
- Transition results distinguish Accepted, AlreadyApplied, Conflict, and NotFound. Invalid movement has typed domain reasons rather than persistence error strings.
- Persistence is authoritative. Runtime publication and dashboard notification happen only after persistence succeeds.
- Runtime publication failure does not roll back durable job state; it produces a diagnostic and reconciliation signal.
- No public runtime-sync switch remains. Publication behavior is inferred from semantic intent.
- Progress logs append atomically. Full log replacement is restricted to compatibility and import adapters.
- Scheduled and job-kind metadata schemas remain owned by their callers; the jobs module owns atomic storage and conflicts.
- Server Action Lifecycle continues to coordinate approval and cancellation. It mutates reversible server state, requests the job transition, compensates on persistence failure, and reconciles on conflict.
- Restart recovery semantically interrupts every nonterminal job with one consistent timestamp and publication cycle.
- Complete historical-record import is a pre-runtime administrative adapter, not an ordinary lifecycle capability.
- Existing job records gain an internal `revision` column defaulted to zero; migration does not rewrite historical facts.
- No ADR is required.

## Target architecture

### Deep module

Deepen `internal/jobs` rather than adding another package. Its small external mutation interface accepts a job identifier and typed transition intent, then returns the accepted record and transition result.

The semantic intent vocabulary covers:

- Start;
- Advance;
- WaitForApproval;
- ResumeAfterApproval;
- Succeed;
- Fail;
- Cancel;
- Interrupt;
- AmendProgress;
- ReplaceMetadata.

The implementation hides:

- the status transition matrix;
- job-kind phase policy and progression;
- start and finish timestamp assignment;
- terminal idempotency and immutability;
- revisioned compare-and-set;
- append-log and metadata persistence mechanics;
- typed conflict classification;
- accepted-record loading;
- runtime publication ordering;
- dashboard notification ordering;
- restart interruption semantics.

### Adapters and callers

- A SQLite adapter owns revision conditions, field persistence, append semantics, and bulk recovery SQL.
- An in-memory adapter provides exhaustive deterministic transition-history tests.
- Runtime Status Projection remains an injected publication adapter rebuilt from accepted persisted jobs.
- Server Action Lifecycle, Scheduled Run, Update runner, scheduled scan, and Backup Operation Lifecycle submit semantic intent and retain their command, audit, and operation ownership.
- A pre-runtime historical import adapter restores complete records without ordinary lifecycle publication.
- Job reports and dashboard consumers continue reading the existing record shape; revision remains internal.

## Delivery slices

### Slice 1: Establish revisioned transitions through one update lifecycle

Add the idempotent revision migration, typed transition intent/result vocabulary, common state machine, timestamp ownership, SQLite compare-and-set, in-memory adapter, and post-persistence publication ordering. Migrate one update job from queued through start, phase advance, and successful completion so the new seam is exercised end to end while other callers remain compatible temporarily.

Verification:

- legal and illegal common transition matrix tests;
- first-start, terminal timestamp, terminal idempotency, and terminal immutability tests;
- two-caller revision race tests against in-memory and SQLite adapters;
- one update lifecycle integration test proving persisted state, runtime publication, dashboard notification, and unchanged report output.

### Slice 2: Migrate update execution, approval, cancellation, and scheduled scan

Move update progress, status synchronization, approval wait/resume, cancellation, failure, scheduled-scan completion, logs, and metadata amendments to semantic transitions. Replace server-status-to-field-patch projection with intent selection and coordinate reversible server state with accepted job results.

Verification:

- update and scheduled-scan lifecycle tests for every terminal outcome;
- deterministic cancel-versus-completion and approval-versus-cancel races;
- persistence-failure compensation and conflict reconciliation tests;
- phase-policy tests for update, autoremove, sudoers, CVE enrichment, and scheduled scan;
- compatibility tests for audit and Runtime Status Projection facts.

### Slice 3: Migrate Scheduled Run and Backup Operation Lifecycle

Move Scheduled Run metadata amendments and Backup Operation Lifecycle progress and outcomes through Job State Transition. Backup success and failure use normal terminal intents instead of complete-record upserts, and rejected late work cannot overwrite an accepted terminal outcome.

Verification:

- Scheduled Run job creation, metadata, reconciliation, and report compatibility tests;
- backup export and restore phase/outcome tests;
- maintenance-release and restore-stage failure tests;
- competing terminal transition tests across Scheduled Run and backup callers;
- unchanged parent-child, audit, and dashboard contracts.

### Slice 4: Own restart interruption and historical import

Replace repository-owned unfinished-job mutation with bulk semantic interruption. Add the restricted pre-runtime import adapter for complete historical records used during restore and initialization. Recovery assigns complete phase, finish timestamp, summary, error class, and revisions, then rebuilds affected runtime state and emits one consolidated notification.

Verification:

- atomic bulk interruption and duplicate-server tests;
- one consistent timestamp and revision increment across recovered jobs;
- runtime rebuild and consolidated notification tests;
- restored legacy database tests with revision absent or zero;
- historical terminal-record preservation and no ordinary import notification tests.

### Slice 5: Remove raw job mutation interfaces

Remove caller-visible raw updates, conditional SQL strings, ordinary complete-record upsert, and runtime-sync choices after all callers cross the semantic seam. Restrict persistence-shaped updates to adapters and add architecture guards preventing old mutation methods and status-to-patch helpers from returning.

Verification:

- architecture tests preventing `UpdateJob`, `UpdateActiveJob`, `UpdateJobWithoutRuntimeSync`, caller-visible `jobs.Update`, and raw repository conditions;
- complete lifecycle compatibility tests for update, scheduled scan, Scheduled Run, backup, restart, reports, dashboard, and audit;
- repository adapter tests proving atomic compare-and-set and append behavior through the production interface.

## Compatibility constraints

- Preserve existing job kinds, statuses, phases, IDs, timestamps, report fields, and JSON shapes.
- Preserve existing parent-child relationships, retry policy JSON, scheduled metadata, audit facts, and dashboard meanings.
- Preserve cancellation winning over late runner completion and restart interruption behavior.
- Keep historical and restored records readable without rewriting their facts.
- Do not expose revision through current HTTP contracts.
- Do not redesign job reports, replace SQLite, introduce distributed queues, cross-process workers, event sourcing, or a workflow engine.
- Do not change Scheduled Run semantics, Server Action Lifecycle command behavior, audit vocabulary, or UI presentation.

## Validation gate

Each slice must pass its focused module, repository-adapter, lifecycle, runtime-publication, report, and architecture tests. The final implementation must also pass:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when all lifecycle callers submit semantic transition intent; the jobs module owns legality, phase rules, timestamps, revisioned persistence, publication, notification, and restart recovery; terminal outcomes resist late work; historical contracts remain compatible; and architecture guards prevent raw patches, raw SQL conditions, and public synchronization choices from returning.
