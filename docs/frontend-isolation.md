# Frontend adapters and isolated browser scenarios

The browser keeps the existing Admin Page Interaction, Scheduled Policy
Administration Interaction, Status Page Interaction, Manage Page Interaction and
Dashboard Projection Consumption boundaries. They own accepted facts, drafts,
selection, request identities, eligibility and command plans. Browser adapters
collect input, execute the plans and render accepted views. Passwords, replacement
notification credentials and uploaded files remain transient browser inputs.

## Responsibility map before extraction

At main `1de9e99173a341719fc28cf764557d4f6f1cc50e`, `admin.js` combined workspace
navigation/timezone controls with notification settings and diagnostics,
account/session/password/activity/metrics effects, backup review/automation and
archive effects, and policy/editor/calendar/run/job-detail effects. Existing
interaction stores already owned their state and decisions.

`index.js` combined Status projection rendering with snapshot/log transport,
selection and table gestures, drawer/password focus, and single-host actions.
`index-bulk-actions.js` used the shell's lexical globals. `manage.js` combined
workspace navigation/observation, inventory/editor/trust/credential effects,
audit filtering/detail/transport, onboarding and import composition. Modal focus
helpers retained page-independent mutable state. Existing onboarding/import and
timezone components had listeners without a complete page-remount contract.

## Ownership after extraction

| Browser module | Responsibility and lifecycle |
| --- | --- |
| `admin-notifications-adapter.js` | Notification form/secret intent DOM, settings/test transport and accepted delivery diagnostics. `create` → `init` → `destroy`; no accepted-state copy. |
| `admin-account-adapter.js` | Password entry aids, session inventory/actions, safe recent activity and Metrics Access Credential effects. Shares the injected Admin Page Interaction owner and command admission. |
| `admin-backup-adapter.js` | Recovery evidence/review rendering, archive export/verify/restore, automation form transport and transient passphrases/files. Restore readiness remains an interaction decision. |
| `admin-policies-adapter.js` | Scheduled Policy Administration Interaction effects: editor inputs, preview debounce, calendars, runs and job details. Destroys its debounce and deferral widget. |
| `status-snapshot-adapter.js` | Snapshot, command-history and generation-aware job-log HTTP effects. Publishes the existing request/stream identities unchanged; the store accepts or rejects replies and plans follow-up pages. |
| `index-bulk-actions.js` | Scoped bulk controls and command-plan execution; consumes the same Status Page Interaction and synchronous snapshot reader as the shell. |
| `status-transport.js` | Idempotent `start`/`init`, SSE, polling and reconnect backoff. `destroy` closes the stream, clears timers and rejects callbacks from superseded connections. |
| `manage-workspace-adapter.js` | Section DOM, navigation/focus, scroll/resize scheduling and ResizeObserver. Section decisions stay in Manage Page Interaction. |
| `manage-audit-adapter.js` | Audit query input/time conversion, accepted table/detail rendering, clipboard and HTTP effects. |
| `manage-modals.js` | Per-page modal focus stack, scroll lock, focus timers and file-label effects. |
| Existing timezone/onboarding/import/deferral components | Their mounts now return destruction hooks; requests, listeners and deferred focus/DOM effects cannot survive page replacement. The application post-check adapter also rejects a response after destruction. |

The shells are explicit compositions, exposed as `AdminPage`, `StatusPage` and
`ManagePage`, each with `mount()` and `destroy()`. Mount destroys the previous
instance and builds fresh adapters around one page interaction owner. `pagehide`
destroys a document that is being unloaded. A persisted back/forward-cache
transition keeps its single mounted instance, preserving accepted selection and
drafts without registering listeners or streams again. On destruction, outstanding
confirmations settle as cancellation, modal state and focus locks close, and old
commands cannot run.
Existing refresh entry points remain compatible. Read-only Manage/Status page
inspection is through `ManagePage.getView()` / `StatusPage.getServerByName()`.

The Admin shell retains workspace/disclosure/timezone composition and event
routing across sections. The Status shell retains coherent table, dashboard,
selected-host and drawer rendering, column/scroll/focus effects and single-host
command orchestration; projection decisions still come from the existing
owners. The Manage shell retains inventory, editor, host-trust and credential
DOM/commands, composing onboarding/import and the extracted workspace/audit
adapters. These are responsibility boundaries, not file-length targets.

`BrowserEffects` only owns listeners, timers, animation frames, observers and
fetch cancellation. Async adapters capture a lifetime and check it after waits
and in legacy catch/finally paths. Conditional confirmation, reauthentication,
file-read and refresh waits also recheck their captured lifetime before command
admission or subsequent effects. Response bodies also reject a superseded
lifetime; an old failure cannot render after destroy/init. External cancellation
listeners detach when a body completes, a request fails or the adapter is
destroyed. Timer namespaces remain independent. Status row selection and import
Configure use stable delegated containers; repeated rendering does not register
per-row cleanups. Pointer listeners and destruction hooks unregister when a
resize gesture ends.

All scripts remain external, same-origin assets, loaded before their consumers.
Changed script cache versions are bumped together. Routes, shell markup, CSS,
Observability theme tokens, English labels and CSP remain compatible.

The #475 synchronous render snapshot scope, cheap bulk preview plans, global
counts/out-of-filter selection and defensive execution plans remain unchanged.
Job recovery retains outage generations, completed-offline jobs, pagination,
coalescing and superseded-reply rejection. Historical #475 performance evidence
still describes its original commits; this change does not relabel those
measurements or claim a new benchmark improvement.

## Browser installation isolation

`tests/e2e/support/fixtures.js` is the only installation/authentication fixture.
Every scenario launches its own `TestDashboardScalingE2EServe` Go test process.
It uses the real router, normal setup/login, role checks, cookies, same-origin
write checks and CSP. It never calls production `main`, starts scheduler or
host-facts workers, or dispatches maintenance runners. SSH connection and host-key
scanning are forbidden by the test composition.

The process binds `127.0.0.1:0` directly; there is no port-probing/rebinding gap.
The fixture accepts readiness only from the spawned PID. Missing, empty or
partially written readiness files are retried within the bounded startup loop;
a foreign PID or listener remains an immediate ownership failure. Each process has an
owned temporary root and a new Go `t.TempDir` DB, adjacent config, known_hosts,
session store, rate limiters and runtime services. Inherited
`DEBIAN_UPDATER_*` / scaling fixture environment is removed. No installed database,
config, destination or real host is used. The browser context closes before the
server receives SIGTERM; the fixture joins the process, checks its global exit,
closes the log and removes the temporary credentials/persistence. A failed
teardown fails the scenario.

Tests share synthetic builders, not browser cookies, passwords, databases or
scenario setup. Authentication uses one known fixture credential; setup tests
start empty, and login tests explicitly create their own account first. Identity,
password and revocation workflows affect only their own installation. Related
steps remain together inside their intentional workflow scenario; no suite uses
`describe.serial`. API builders provide accepted synthetic wire facts rather
than acting as an independent implementation of backend policy.

The 99 existing scenarios are split across auth, Status/approval,
Manage/onboarding, Admin/policies/backup, Observability and identity specs. Five
additional scenarios cover complete page remounts, simultaneous installations
and import focus during viewport changes.
The import adapter executes accepted focus effects against actual keyboard-focus
ownership, so a layout-driven navigation-highlight change cannot swallow result
focus. A newer navigation/editor focus target remains protected.

The simultaneous-installation test checks distinct PIDs/ports/DB/config/trust
paths and proves password rotation plus revocation cannot invalidate the other
installation's sessions or original credential.

## Reproducible commands

Install locked tooling and Chromium, then run the suite. No installed server is
needed; global setup compiles one disposable Go test executable, shared only as
immutable executable code. All scenario state remains independent.

```bash
npm ci
npx playwright install chromium
npm run test:unit
npm run test:e2e -- --workers=1 --retries=0
npm run test:e2e -- --workers=4 --retries=0
```

Representative independent invocations (each must discover and execute a test):

```bash
npm run test:e2e -- tests/e2e/auth.spec.js --grep 'invalid login' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/status.spec.js --grep 'pending updates drawer keeps scroll' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/manage.spec.js --grep 'onboarding failed checks' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/admin.spec.js --grep 'admin typed confirmations gate' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/observability.spec.js --grep 'keeps a successful summary' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/identity.spec.js --grep 'confirmed MFA challenge' --workers=1 --retries=0
npm run test:e2e -- tests/e2e/lifecycle.spec.js --workers=2 --retries=0
```

For repeated qualification, build the test composition once into an owned output
directory, then pass it to the independent-scenario runner or capture tool:

```bash
mkdir -p /tmp/slu-browser-validation
go test -c -o /tmp/slu-browser-validation/server.test .
node tools/frontend/independent-scenarios.cjs /tmp/slu-browser-validation/server.test /tmp/slu-browser-validation/individual
node tools/frontend/capture.cjs /tmp/slu-browser-validation/server.test /tmp/slu-browser-validation/screenshots
node tools/performance/dashboard-e2e.cjs /tmp/slu-browser-validation/server.test /tmp/slu-browser-validation/compatibility --grep 'invalid login'
```

The independent runner discovers the whole suite, launches a separate Playwright
invocation per title (two bounded invocation workers), and requires exactly one
passed scenario, no skips, no retries and global exit zero for each invocation.
It writes a sanitized numeric/title summary plus private raw reports.

`SLU_E2E_TEST_BINARY` optionally supplies the already-built **test executable**.
`SLU_E2E_OUTPUT_DIR` controls browser artifacts; its default is under the OS temp
directory. `PLAYWRIGHT_HTML_OUTPUT_DIR` controls CI's HTML report destination.
CI and the trusted release gate explicitly preserve their existing report/artifact
paths. `PLAYWRIGHT_WEBSERVER_COMMAND` is retired and rejected with migration
instructions; `SLU_IDENTITY_TEST_BINARY` is no longer used. The #475
`dashboard-e2e.cjs binary output [Playwright arguments...]` entry point remains
functional and reports the Playwright global exit in `lifecycle.json`; per-test
fixture teardown now owns server exits instead of an unused global server. The
existing `server.code` field remains zero when the complete successful suite
proves every fixture teardown succeeded; it is unknown (`null`) after a failed
suite. This keeps successful legacy lifecycle checks compatible without claiming
a global server still exists.

The capture tool uses the same fixed browser clock (`2026-05-28T12:00:00Z`),
UTC/en-US context, synthetic facts and desktop/mobile sizes. It rejects page
exceptions, console errors and horizontal page overflow. Keep raw traces and
cookie-bearing logs in an owned private temporary directory. Only inspected
synthetic screenshots and sanitized summaries belong in git.

See [the #476 validation record](validation/frontend-isolation-476/README.md)
for source provenance, actual counts/exits, screenshots and material limits.
