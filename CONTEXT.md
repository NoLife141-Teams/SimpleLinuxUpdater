# SimpleLinuxUpdater

SimpleLinuxUpdater manages Debian and Ubuntu host maintenance from a local web application. This glossary names the product concepts that should stay consistent across routes, jobs, audit records, scheduled policies, and dashboard views.

## Language

**Server Action Lifecycle**:
The lifecycle of an operator-initiated server action from request validation through runtime-state transition, job persistence, runner dispatch, approval or cancel transition, audit recording, rollback, and final response mapping.
_Avoid_: Update route flow, action handler logic

**Server Inventory Command**:
The operator-facing command vocabulary for creating, changing, deleting, and trust-managing server inventory entries, including command outcome, audit facts, and operator-facing messages.
_Avoid_: Server route logic, inventory HTTP handler

**Approval Scope**:
The operator's chosen package set for a pending update, including whether it targets all standard upgrades, standard security upgrades, kept-back security upgrades, or a full upgrade, and whether package removals have been confirmed.
_Avoid_: Approval intent, upgrade mode

**Scheduled Run**:
One scheduler-created attempt to apply a scheduled update policy to one server for one scheduled time, including skipped, queued, running, waiting-approval, succeeded, failed, cancelled, and interrupted outcomes.
_Avoid_: Scheduled policy execution, policy job

**Job State Transition**:
An accepted change to a persisted job's lifecycle state, including its status, phase, progress facts, terminal outcome, and runtime publication, governed by transition and concurrency rules.
_Avoid_: Job update patch, status write, job field update

**Policy Schedule Projection**:
The read-only interpretation of scheduled policy matching, cadence, no-run windows, candidate priority, and future scheduled-run state into per-server schedule facts consumed by Dashboard Projection and calendar-facing views.
_Avoid_: Dashboard schedule helper, next-run calculation

**Scheduled Policy Administration Interaction**:
The operator-facing interpretation of a scheduled policy draft, its matching preview, no-run windows, calendar and Scheduled Run views, and selected job detail into the state and decisions needed to administer scheduled maintenance.
_Avoid_: Admin page globals, policy editor event handlers

**Admin Page Interaction**:
The application-level interpretation of accepted administration facts and operator intents into deterministic timezone, notification, account-session, metrics-token, backup, and Scheduled Policy Administration Interaction views and command plans.
_Avoid_: Admin page globals, admin DOM state, settings event handlers

**Dashboard Projection**:
The derived operational view that combines current server state, maintenance activity, scheduled availability, health facts, update history, approval triage, and fleet counters into the dashboard summary shown to operators.
_Avoid_: Dashboard API response, observability query

**Dashboard Projection Consumption**:
The application-level interpretation of accepted Server and Dashboard Projection facts into deterministic operator-facing server, fleet, attention, approval, schedule, activity, command-history, and selected-host views.
_Avoid_: Dashboard HTML helpers, frontend summary mapping

**Dashboard Action Contract**:
The Dashboard Projection-owned action eligibility vocabulary for dashboard-visible server actions, including whether each action is enabled, why it is ready or blocked, its machine-readable readiness state, blocking status, and action-specific counts.
_Avoid_: Button state logic, frontend action rules

**Status Page Interaction**:
The application-level state and decision boundary for status-page snapshots, navigation, synchronization, selection, drawer state, and single or bulk action planning; browser adapters retain transport, persistence, DOM, focus, scrolling, and confirmation effects.
_Avoid_: Status page globals, dashboard UI state

**Manage Page Interaction**:
The operator-facing interpretation of server inventory, editor sessions, host-key trust, policy-override visibility, global-key availability, audit query/results, and selected audit detail into the state and decisions used to administer servers.
_Avoid_: Manage page globals, inventory handler flow

**Observability Page Interaction**:
The operator-facing interpretation of observability window selection, health-trend host selection, independently refreshed snapshots, partial failure, visibility, and refresh lifecycle into one accepted page view.
_Avoid_: Observability globals, polling helpers, observability DOM state

**Health Snapshot Capture**:
The accepted recording of time-ordered Server health observations from collected facts or completed maintenance, including package counts, update or scan outcome, disk, APT, reboot, source, and retention.
_Avoid_: Audit health callback, health-history write, snapshot row

**Runtime Status Projection**:
The derived interpretation of runtime server status, job status, job phase, and scheduled-run/job reconciliation state into operation progress, action blocking, and dashboard timeline state.
_Avoid_: Status helper logic, dashboard status mapping

**Package Discovery and Upgrade Plan**:
The read-only discovery of pending Debian/Ubuntu package updates after package metadata refresh, including standard upgrade simulation, full-upgrade simulation, apt metadata enrichment, kept-back security simulation, and the derived package/update plan consumed by Approval Scope, Scheduled Run, and Dashboard Projection.
_Avoid_: Apt helper parsing, upgradable list helper

**Backup Operation Lifecycle**:
The lifecycle of an operator-initiated backup export or restore from accepted request through active-action gating, job persistence, maintenance mode, archive execution, audit recording, restored-runtime handoff, session invalidation, and final operation outcome.
_Avoid_: Backup route flow, restore handler logic

**Maintenance Coordination**:
The app-wide admission and exclusivity model that allows ordinary work to proceed concurrently, grants one exclusive maintenance operation at a time, projects its public state, and preserves that state across persistence replacement and startup recovery. It is separate from Server maintenance and scheduled policy no-run windows.
_Avoid_: Backup lock, maintenance flag, restore barrier

**Application Time Interpretation**:
The app-wide interpretation of a configured or system-local timezone into effective local civil time, UTC instants, and operator-facing timestamps used consistently by Scheduled Run, audit, and projections.
_Avoid_: App timezone helpers, local time callbacks, timezone formatting

**Auth Session Command**:
An operator-facing authentication mutation that coordinates account state, credential validation, session staging or destruction, rate-limit policy, audit facts, partial completion, and a transport-neutral outcome for setup, login, logout, password change, or session clearing.
_Avoid_: Auth route flow, session handler logic

**Runtime Composition**:
The assembly and restored-state rehydration of one app-scoped runtime from persistence, server state, services, job and session managers, eventing, rate limiters, clocks, maintenance state, and startup initializers.
_Avoid_: AppDeps wiring, dependency injection container

**Global SSH Credential**:
The optional app-wide SSH private key that Host Maintenance Session uses only when a Server has no per-server SSH key. It does not replace per-server credentials or host-key trust.
_Avoid_: Global key, fallback key, shared key

**Host Maintenance Session**:
A bounded, authenticated, host-key-verified execution context for performing maintenance capabilities against one server, including SSH connection establishment, command timeout, reconnect, retry accounting, and transport closure, while the invoking application service owns action lifecycle, approval, jobs, audit, and persistence.
_Avoid_: SSH helper bundle, update connection
