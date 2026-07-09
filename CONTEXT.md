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

**Policy Schedule Projection**:
The read-only interpretation of scheduled policy matching, cadence, no-run windows, candidate priority, and future scheduled-run state into per-server schedule facts consumed by Dashboard Projection and calendar-facing views.
_Avoid_: Dashboard schedule helper, next-run calculation

**Dashboard Projection**:
The derived operational view that combines current server state, maintenance activity, scheduled availability, health facts, update history, approval triage, and fleet counters into the dashboard summary shown to operators.
_Avoid_: Dashboard API response, observability query

**Dashboard Action Contract**:
The Dashboard Projection-owned action eligibility vocabulary for dashboard-visible server actions, including whether each action is enabled, why it is ready or blocked, its machine-readable readiness state, blocking status, and action-specific counts.
_Avoid_: Button state logic, frontend action rules

**Status Page Interaction**:
The application-level state and decision boundary for status-page snapshots, navigation, synchronization, selection, drawer state, and single or bulk action planning; browser adapters retain transport, persistence, DOM, focus, scrolling, and confirmation effects.
_Avoid_: Status page globals, dashboard UI state

**Runtime Status Projection**:
The derived interpretation of runtime server status, job status, job phase, and scheduled-run/job reconciliation state into operation progress, action blocking, and dashboard timeline state.
_Avoid_: Status helper logic, dashboard status mapping

**Package Discovery and Upgrade Plan**:
The read-only discovery of pending Debian/Ubuntu package updates after package metadata refresh, including standard upgrade simulation, full-upgrade simulation, apt metadata enrichment, kept-back security simulation, and the derived package/update plan consumed by Approval Scope, Scheduled Run, and Dashboard Projection.
_Avoid_: Apt helper parsing, upgradable list helper

**Backup Operation Lifecycle**:
The lifecycle of an operator-initiated backup export or restore from accepted request through active-action gating, job persistence, maintenance mode, archive execution, audit recording, restored-runtime handoff, session invalidation, and final operation outcome.
_Avoid_: Backup route flow, restore handler logic

**Auth Session Command**:
An operator-facing authentication mutation that coordinates account state, credential validation, session staging or destruction, rate-limit policy, audit facts, partial completion, and a transport-neutral outcome for setup, login, logout, password change, or session clearing.
_Avoid_: Auth route flow, session handler logic

**Runtime Composition**:
The assembly of one app-scoped runtime from persistence, server state, services, job and session managers, eventing, rate limiters, clocks, maintenance state, and startup initializers.
_Avoid_: AppDeps wiring, dependency injection container
