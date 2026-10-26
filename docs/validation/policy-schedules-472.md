# Policy schedules #472 validation

Validated on 2026-10-26 against base `e842af306b8e9fe56f3af3ae1cd8a3199d354805`.
Go 1.26.10, staticcheck 2026.1 (v0.7.0), one Playwright Chromium worker.
No release version changed.

## Delivered behavior

- Canonical one-time local dates and monthly numeric/last-day schedules, shared
  by Go preview, conflict detection, calendar, Dashboard Projection and dispatch.
  Short months are skipped with a diagnostic. DST gaps are skipped and overlaps
  choose the earlier UTC instant through Application Time Interpretation.
- Atomic persisted occurrence manifests before dispatch: policy revision,
  configuration, original UTC origin and complete names/batches, without secrets.
  One-time consumption is a policy-lifetime latch, including across cadence,
  date/time, timezone and target edits, restart and waiting approval.
- Explicit canaries block visibly when missing, disabled, excluded or overridden.
  Automatic selection retains its previous default name order. Current inventory
  availability, overrides, maintenance and no-run windows still guard each start.
- Accepted rollouts keep their revision and composition after policy/inventory
  changes. Failed/skipped canaries, including blocking application postchecks,
  never release waves. Newly recorded runs expose revision/stage/wave metadata.
- Additive schema migration, real encrypted archive export/restore round-trip,
  English Admin editor/preview/calendar/history and responsive fixture captures.

## Checks

All final commands below completed with exit code 0. Go commands used
`GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache` and `GOMAXPROCS=2`.
Staticcheck also used `XDG_CACHE_HOME=/tmp/slu-472-analysis-cache`.

| Command | Result |
| --- | --- |
| `go test ./...` | Passed; final root package 54.224 s. |
| `go test -race -count=1 ./...` | Passed; root package 175.322 s; no race report. |
| `go test -count=1 -covermode=atomic -coverprofile=/tmp/slu-472-coverage.out ./...` | Passed; total **75.9%**, CI minimum **73.0%**; policies package 77.6%. |
| `go vet ./...` | Passed. |
| `go build ./...` | Passed. |
| `staticcheck ./...` | Passed with v0.7.0. |
| `npm run test:unit` | Passed. |
| `npm run test:e2e -- --grep 'policy schedules 472' --workers=1` | Three scenarios passed, including added calendar/history assertions. |
| `npm run test:e2e -- --workers=1` | **91 passed**, 2.0 min, final process exit **0**. |
| `node --check` for changed JS; `gofmt -l`; `git diff --check` | Passed. |

Go coverage includes month ends, leap February, spring gaps/fall overlaps,
consumption through restart/timezone/cadence changes, missed-tick recovery,
conflicts/priority, blackouts, unavailable named canaries, frozen active rollouts,
upgrade of daily/weekly policies and a pre-upgrade active rollout. The real
Scheduled Run runner asserts accepted policy/job/audit metadata. Application
postcheck runner tests cover blocking and warning results for daily, one-time
and monthly schedules. API tests cover creation, edits, preview and calendar;
the encrypted backup test restores consumption, monthly configuration and the
unchanged accepted manifest and proves it cannot dispatch a late canary.

The complete Playwright suite exercises the shared Admin interaction as well as
Status, Manage, notifications, backup/recovery and inventory import. The three
new scenarios cover one-time and monthly drafts, last-day controls, explicit
canaries, server-owned projections, consumption after cadence edits, navigation
preservation and 390 px layout. Six screenshots are in
[`.github/assets/policy-schedules-472`](../../.github/assets/policy-schedules-472/README.md).

## Harness notes and limits

The first unconstrained full Go run hit the existing account-recovery PTY
helper's 15-second child-process timeout while packages competed for CPU. The
bounded final standard, race and coverage suites all passed. Staticcheck's
initial default cache was outside the writable sandbox; redirecting its cache
to `/tmp` resolved that environment error.

The first full browser report showed 91 passing scenarios, but its temporary
server shell returned signal 143 during teardown. The temporary launcher was
corrected to replace the shell with the app process using `exec`; the full suite
was rerun on the final tree and its exit code 0 recorded. No production shutdown
code or CSS was changed for this harness correction or for screenshot framing.

Tests used disposable databases/configuration/encryption files, localhost
listeners, simulated Host Maintenance Sessions and API fixtures. No real SSH
host, package manager, user inventory or user volume was modified; no live SSH
or APT smoke is claimed. Existing user files under `static/prototypes/` retain
all three initial SHA256 hashes and are excluded from the commit. Stashes were
not touched.

The original complete membership of an unrecorded pre-upgrade wave cannot be
recovered; its first observation freezes then-eligible members and retains every
persisted predecessor as a gate. An older backup cannot recover operations
performed after export. These limits and the policy-lifetime consumption rule
are documented in [usage](../usage.md#policy-schedule-cadences-and-recovery).
Issue #473 package deferrals and automatic fleet reboots remain out of scope.
