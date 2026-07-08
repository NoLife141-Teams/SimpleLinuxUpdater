# SimpleLinuxUpdater

SimpleLinuxUpdater manages Debian and Ubuntu host maintenance from a local web application. This glossary names the product concepts that should stay consistent across routes, jobs, audit records, scheduled policies, and dashboard views.

## Language

**Server Action Lifecycle**:
The lifecycle of an operator-initiated server action from request validation through runtime-state transition, job persistence, runner dispatch, approval or cancel transition, audit recording, rollback, and final response mapping.
_Avoid_: Update route flow, action handler logic

**Approval Scope**:
The operator's chosen package set for a pending update, including whether it targets all standard upgrades, standard security upgrades, kept-back security upgrades, or a full upgrade, and whether package removals have been confirmed.
_Avoid_: Approval intent, upgrade mode

**Scheduled Run**:
One scheduler-created attempt to apply a scheduled update policy to one server for one scheduled time, including skipped, queued, running, waiting-approval, succeeded, failed, cancelled, and interrupted outcomes.
_Avoid_: Scheduled policy execution, policy job

**Dashboard Projection**:
The derived operational view that combines current server state, maintenance activity, scheduled availability, health facts, update history, approval triage, and fleet counters into the dashboard summary shown to operators.
_Avoid_: Dashboard API response, observability query

**Runtime Status Projection**:
The derived interpretation of runtime server status, job status, job phase, and scheduled-run/job reconciliation state into operation progress, action blocking, and dashboard timeline state.
_Avoid_: Status helper logic, dashboard status mapping

**Runtime Composition**:
The assembly of one app-scoped runtime from persistence, server state, services, job and session managers, eventing, rate limiters, clocks, maintenance state, and startup initializers.
_Avoid_: AppDeps wiring, dependency injection container
