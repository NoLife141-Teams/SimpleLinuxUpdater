# Observability Page Interaction implementation plan

## Outcome

Deepen Observability Page Interaction into one deterministic browser module that owns accepted observability snapshots, selected window and health-trend host, independent freshness and failure state, stale-response rejection, and the visible-page refresh lifecycle. Keep transport, timers, document visibility, timezone loading, logout, and DOM mutation in browser adapters while preserving the current HTTP, visual, timestamp, control, and 15-second refresh contracts.

## Domain decisions

- Observability Page Interaction is the canonical owner of the accepted page view and refresh decisions.
- Summary and health-trend snapshots begin in one full refresh generation but settle and render independently.
- A successful source publishes immediately even when the other source is pending or failed.
- A failed source retains its last accepted snapshot and becomes stale; it is unavailable only when no accepted snapshot exists.
- Loading never clears accepted data.
- Each source result carries a request identity within a monotonically increasing generation; obsolete results are ignored.
- Transport cancellation is an optimization, not the correctness mechanism. Generation checks remain authoritative.
- A selected-window change supersedes summary and health-trend work. A selected-host change supersedes health-trend work only.
- Health trends retain the existing `24h` selection to `7d` query-window compatibility rule.
- Known host choices come only from the latest successful unfiltered health-trend snapshot. Filtered, failed, or aborted results cannot erase them.
- A selected host falls back to All hosts only when a successful unfiltered snapshot proves it is absent.
- Automatic refresh is completion-aware: schedule the next full refresh 15 seconds after the active full generation settles, with no overlapping automatic generations.
- Manual refresh starts a new full generation, supersedes active work, retains accepted data, and resets automatic scheduling after settlement.
- Hidden-page transition cancels scheduled and active work without producing operator-visible errors. Visibility restoration starts exactly one full refresh.
- Page state remains in memory for the page lifetime; no local storage, URL state, cross-tab synchronization, or persisted browser cache is introduced.
- The interaction returns semantic view facts and requested effects, never HTML or DOM nodes.
- Domain labels and source freshness belong to the interaction; generic number, disk, duration, CSS, table, and timestamp presentation remain adapter concerns.
- Existing backend endpoints and response fields remain unchanged.
- No ADR, backend migration, schema change, or visual redesign is required.

## Target architecture

### Deep module

Add one browser-side Observability Page Interaction module. Its external interface accepts operator, lifecycle, and source-result events and returns a transition containing:

- one immutable accepted view;
- zero or more requested effects;
- request identities needed to correlate later results.

The accepted view contains selected window and host, known host choices, accepted summary and health-trend snapshots, per-source freshness, structured failure facts, refresh activity, last-accepted facts, and control availability.

Requested effects use a small vocabulary such as loading summary, loading health trends, scheduling or cancelling refresh, and aborting obsolete transport. The interface does not expose separate setters for snapshots, errors, loading flags, generations, or selections.

The implementation hides:

- full and source-specific generation rules;
- independent source settlement;
- stale and unavailable classification;
- accepted-snapshot retention;
- host-choice reconciliation;
- the `24h` to `7d` trend mapping;
- manual, automatic, selection, visibility, and initial-load refresh semantics;
- aborted-result suppression;
- completion-aware scheduling decisions.

### Adapters

- The production browser adapter translates DOM events and visibility changes into interaction events.
- The fetch adapter builds the two existing endpoint requests, attaches abort signals, and returns structured success or failure results with request identities.
- The timer adapter executes requested completion-aware scheduling effects.
- The DOM adapter renders semantic view facts using the current markup, CSS classes, number formatting, table rendering, and application-time browser adapter.
- A deterministic test adapter supplies controlled results, ordering, visibility, and clock effects through the same interface used in production.

The Go observability module continues to own server-side queries and projection construction. Dashboard Projection and Dashboard Projection Consumption are unchanged.

## Delivery slices

### Slice 1: Establish accepted full-refresh interaction

Introduce the deterministic interaction interface, initial state, selected window, accepted summary and health-trend snapshots, request identities, and a full-refresh transition. Route initial page load, manual refresh, and window changes through the module while retaining current rendering and endpoint contracts.

The production adapter starts summary and health-trend requests concurrently. Each successful source can update the accepted view as soon as it returns, while existing content remains visible during later refreshes.

Verification:

- interaction tests for initial state, initial visible load, concurrent effect requests, either completion order, immutable transitions, retained data while loading, manual refresh, and selected-window supersession;
- adapter tests for unchanged endpoint parameters, `24h` to `7d` health-trend mapping, timezone readiness, and current rendering output;
- Playwright coverage for initial rendering, manual refresh, and window changes.

### Slice 2: Own independent failure and host selection

Add per-source fresh, refreshing, stale, and unavailable state with structured transport, HTTP, decoding, and aborted results. Preserve successful or previously accepted data during partial failure and render source-specific operator warnings without clearing the other source.

Move health-trend host selection and known-host reconciliation into the interaction. Host changes reload health trends only. Successful unfiltered snapshots maintain the full host choice list; filtered, failed, or aborted results cannot clear it.

Verification:

- interaction tests for every independent success/failure order, retained stale data, unavailable first load, source-specific error clearing, silent aborts, and structured failure facts;
- host-selection tests for trends-only refresh, filtered results, disappeared hosts, failed refreshes, and stable All hosts fallback;
- Playwright coverage for partial failure with retained data and coherent host selection.

### Slice 3: Own stale-response and refresh lifecycle

Complete generation-based stale-response rejection, transport cancellation requests, completion-aware automatic scheduling, no-overlap guarantees, and hidden/visible lifecycle behavior. Replace the permanent interval with a timeout scheduled 15 seconds after a full generation settles.

Manual refresh and selection changes supersede only their intended scope. Hidden-page transition cancels active and scheduled work silently while retaining accepted state; visibility restoration starts one full refresh and resumes scheduling after settlement.

Remove obsolete page globals and acceptance logic from the browser adapter. Add an architecture guard that keeps refresh generations, accepted snapshots, host reconciliation, and source freshness behind Observability Page Interaction.

Verification:

- interaction tests for out-of-order responses, obsolete success and failure rejection, manual-versus-automatic supersession, source-specific supersession, no overlapping automatic generations, settlement-based delay, hidden cancellation, and one restoration refresh;
- adapter tests for abort execution, timer replacement, visibility events, and absence of operator errors for cancelled work;
- Playwright coverage for out-of-order response protection and refresh lifecycle where browser timing is deterministic;
- architecture tests preventing mutable interaction-state globals and snapshot-acceptance rules from returning to the DOM adapter.

## Compatibility constraints

- Keep `/api/observability/summary` and `/api/observability/health-trends` unchanged.
- Keep existing response fields and application-time display behavior unchanged.
- Keep the current window choices and the health-trend `24h` to `7d` mapping.
- Keep the 15-second automatic refresh cadence, now measured from settlement.
- Keep All hosts behavior, KPI values, tables, empty-state meaning, CSS classes, responsive layout, navigation, and logout behavior.
- Keep accepted content visible during refresh rather than replacing it with loading placeholders.
- Do not introduce persistent page state, new backend aggregation, new observability calculations, WebSocket or SSE transport, cross-tab behavior, or a frontend framework.
- Do not refactor unrelated Dashboard Projection, Application Time Interpretation, backend observability queries, or Playwright scenarios.

## Validation gate

Each slice must pass its focused deterministic module, browser-adapter, DOM rendering, and Playwright tests. The final implementation must also pass:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when one Observability Page Interaction module owns accepted state and refresh decisions; browser adapters contain effects but no snapshot-acceptance rules; sources settle and fail independently; obsolete results cannot overwrite newer choices; automatic refresh never overlaps; hidden cancellation is silent; host choices remain coherent; compatibility contracts remain intact; and architecture guards preserve the seam.
