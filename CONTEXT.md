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
