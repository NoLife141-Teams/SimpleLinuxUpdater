# SimpleLinuxUpdater

SimpleLinuxUpdater manages Debian and Ubuntu host maintenance from a local web application. This glossary names the product concepts that should stay consistent across routes, jobs, audit records, scheduled policies, and dashboard views.

## Language

**Server Action Lifecycle**:
The lifecycle of an operator-initiated server action from request validation through runtime-state transition, job persistence, runner dispatch, approval or cancel transition, audit recording, rollback, and final response mapping.
_Avoid_: Update route flow, action handler logic
