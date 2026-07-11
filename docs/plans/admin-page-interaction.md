# Admin Page Interaction implementation plan

## Outcome

Deepen Admin Page Interaction into one deterministic browser module that owns accepted administration facts, operator intent, request freshness, command availability, feedback state, and coordination with Scheduled Policy Administration Interaction. Keep fetch, DOM mutation, confirmation, clipboard, downloads, file input, timers, application-time caching, and navigation in browser adapters while preserving every current endpoint, payload, message, visual, accessibility, and refresh contract.

## Domain decisions

- Admin Page Interaction is the canonical owner of the accepted page view and administration command plans.
- The page module composes Scheduled Policy Administration Interaction; it does not absorb or duplicate scheduled-policy draft, preview, calendar, run, or selected-job semantics.
- Each independently loaded Admin concern has its own request identity, accepted snapshot, freshness, and failure state.
- Obsolete successes and failures are ignored even if transport cancellation is unavailable.
- Loading retains the last accepted snapshot. A failed refresh makes existing data stale and is unavailable only when no accepted snapshot exists.
- A command is planned from accepted state and operator intent, then marked in flight until its correlated result settles.
- Repeated commands are rejected while the same semantic command is in flight.
- Successful mutations request only the smallest refresh set needed to reconcile accepted state.
- Timezone state contains both the configured value and resolved Application Time Interpretation returned by the backend.
- Saving timezone requests reconciliation of timezone-dependent Scheduled Policy Administration Interaction views without reimplementing schedule semantics.
- Notification settings own enabled state, webhook URL, selected event types, last-delivery facts, validation feedback, and save/test command availability.
- Account administration owns accepted session count, password-change draft state, clear-session and password-change command availability, and outcome feedback. Credential values are never returned by the accepted view after command planning.
- Metrics-token administration owns configured and one-time-reveal facts. The revealed token remains page-memory-only and is cleared on explicit hide, disable, navigation, or replacement.
- Backup administration owns accepted availability and maintenance facts, selected file metadata, command availability, progress, and outcome feedback. Backup bytes, passphrases, object URLs, downloads, and file contents remain adapter-only.
- Scheduled Policy Administration Interaction remains responsible for scheduled policy drafts, previews, settings, calendar, runs, job detail, and schedule-specific effects. Admin Page Interaction coordinates its visibility, timezone facts, command lifecycle, and page-level refresh effects.
- The module returns semantic views and requested effects, never DOM nodes, HTML, browser events, `Response` objects, files, timers, or abort controllers.
- Existing backend endpoints, HTTP methods, request and response fields, security checks, audit facts, and error wording remain unchanged.
- No backend, schema, template-layout, or visual redesign is required.
- No ADR is required because this follows the established Status Page Interaction, Manage Page Interaction, and Observability Page Interaction seam.

## Target architecture

### Deep module

Add one browser-side Admin Page Interaction module with the small interface already proven by other page interactions:

- `createStore(options)` creates page-lifetime state;
- `dispatch(event)` accepts lifecycle, snapshot, draft, and command-result events and returns requested effects;
- `getView()` returns an immutable semantic view;
- `planCommand(command, payload)` returns an immutable command plan without executing browser effects.

The implementation hides:

- per-concern request identities and stale-response rejection;
- accepted snapshot retention and fresh, refreshing, stale, or unavailable classification;
- command deduplication and correlated settlement;
- panel-specific draft normalization and validation;
- feedback lifecycle and partial-failure behavior;
- least-refresh reconciliation after mutations;
- timezone-to-schedule reconciliation decisions;
- metrics-token reveal lifetime;
- backup command eligibility and page-level progress;
- composition with Scheduled Policy Administration Interaction.

The accepted view contains semantic panel views for timezone, notifications, account administration, metrics token, backup, and scheduled maintenance, plus page-level streams and in-flight commands. Secret inputs and binary data are excluded.

### Adapters

- The production browser adapter translates DOM and page lifecycle events into interaction events.
- The transport adapter executes existing fetch requests and returns structured success, failure, or aborted results with request and command identities.
- The DOM adapter renders semantic views using current markup, CSS classes, focus behavior, ARIA feedback, tables, modals, and formatting helpers.
- Confirmation, clipboard, backup download, restore file reading, passphrase collection, and application-time caching remain browser adapters.
- Scheduled Policy Administration Interaction remains an internal collaborator supplied to or created by the page module; its current public interface and tests remain valid.
- A deterministic test adapter supplies controlled request ordering, command results, and partial failures through the production interface.

## Delivery slices

### Slice 1: Establish Admin Page Interaction through timezone administration

Introduce the page module, immutable view, request and command correlation, freshness vocabulary, and effect runner. Migrate timezone load, edit, save, feedback, and timezone-dependent scheduled-view reconciliation through the new seam while retaining existing DOM and Application Time Interpretation adapters.

Verification:

- deterministic tests for initial state, accepted timezone facts, configured versus resolved timezone, retained data during refresh, stale response rejection, save deduplication, success and failure feedback, and minimal schedule reconciliation effects;
- adapter tests for the existing timezone endpoint, payload, cache integration, picker behavior, and rendered feedback;
- Playwright coverage for loading and saving a timezone with scheduled summaries and runs remaining coherent.

### Slice 2: Add notification administration

Move notification settings, event selection, last-delivery facts, validation feedback, save planning, test-delivery planning, command deduplication, and result reconciliation behind Admin Page Interaction. Keep webhook transport and DOM controls in adapters.

Verification:

- deterministic tests for settings normalization, accepted last-delivery facts, save and test command plans, independent failures, stale response rejection, duplicate-command rejection, and successful snapshot reconciliation;
- adapter tests for unchanged notification endpoints, payloads, supported-event rendering, and error wording;
- Playwright coverage for saving settings, sending a test, and retaining accepted settings when a refresh fails.

### Slice 3: Add account-session and metrics-token administration

Move session-count snapshots, password-change draft decisions, clear-session commands, metrics-token status, rotate, reveal, copy eligibility, hide, and disable decisions behind Admin Page Interaction. Keep password values, token clipboard writes, confirmations, session transport, and navigation effects in adapters.

Verification:

- deterministic tests for password and confirmation validation, secret removal after planning, clear-session command lifecycle, partial account failure, token rotate/disable deduplication, one-time reveal replacement, explicit clearing, and copy eligibility;
- adapter tests for unchanged auth and metrics-token endpoints, confirmation behavior, clipboard effects, and logout/session consequences;
- Playwright coverage for password feedback, session clearing, token rotation/copy/disable, and one-time reveal behavior.

### Slice 4: Add backup administration

Move backup status, selected-file metadata, export, verify, and restore command availability, in-flight progress, and outcome feedback behind Admin Page Interaction. Keep passphrases, archive bytes, file handles, object URLs, downloads, typed confirmation, and restore transport in adapters.

Verification:

- deterministic tests for maintenance and active-action eligibility, file-metadata acceptance, missing-input rejection, command deduplication, progress, failure retention, verify outcomes, restore outcomes, and minimal post-command refresh effects;
- adapter tests proving secret and binary data never enter the accepted view, downloads are revoked, file input remains browser-owned, and existing backup endpoints and messages remain unchanged;
- Playwright coverage for export, verify, restore validation, blocked states, and feedback using controlled transport.

### Slice 5: Compose scheduled administration and remove leaked page state

Route page-level scheduled loading, timezone propagation, command lifecycle, refresh coordination, and selected-job visibility through Admin Page Interaction while leaving scheduled-policy semantics in Scheduled Policy Administration Interaction. Remove obsolete page globals and acceptance rules from the browser adapter, then add architecture guards preserving the seam.

Verification:

- composition tests for scheduled effects, timezone propagation, independent scheduled streams, partial failure, command settlement, selected-job state, and unchanged Scheduled Policy Administration Interaction behavior;
- adapter tests for current policy, settings, preview, calendar, run, and job-detail endpoints and rendering;
- architecture tests preventing accepted Admin state, request identities, command-in-flight state, and cross-panel reconciliation rules from returning to the DOM adapter;
- Playwright coverage for policy editing, preview, calendar, run detail, timezone changes, and partial failure across Admin panels.

## Compatibility constraints

- Keep all existing Admin endpoints, HTTP methods, JSON fields, multipart fields, download filenames, and authentication, same-origin, maintenance, and rate-limit behavior unchanged.
- Keep current timezone choices, configured/resolved timezone semantics, Application Time Interpretation cache behavior, and scheduled-view results unchanged.
- Keep notification supported events, webhook validation, retry behavior, last-delivery fields, and operator-facing messages unchanged.
- Keep password policy, session invalidation, metrics-token one-time reveal, bearer-token behavior, and confirmation wording unchanged.
- Keep backup archive format, size limits, passphrase handling, typed confirmation, export download, verify behavior, restore handoff, and session invalidation unchanged.
- Keep Scheduled Policy Administration Interaction semantics, policy payloads, preview behavior, no-run windows, calendar, run and job-detail rendering, and polling behavior unchanged.
- Keep existing markup, CSS classes, focus behavior, accessibility announcements, responsive layout, navigation, and logout behavior.
- Do not add persistent browser state, a frontend framework, cross-tab synchronization, new backend aggregation, schema changes, or new product behavior.
- Do not refactor unrelated Status, Manage, Observability, Dashboard Projection, backend lifecycle, or persistence modules.

## Validation gate

Each slice must pass its focused deterministic module, adapter, DOM, and Playwright tests. The final implementation must also pass:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when one Admin Page Interaction module owns accepted administration state, freshness, command planning, feedback, and page-level reconciliation; Scheduled Policy Administration Interaction remains the schedule semantic collaborator; browser adapters contain effects but no acceptance rules; stale results cannot overwrite newer intent; secrets and binary data never enter the accepted view; current product contracts remain unchanged; and architecture guards preserve the seam.
