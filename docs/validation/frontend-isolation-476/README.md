# Frontend isolation #476 validation

Production implementation: **18881ee209d45554583871337c44376c36aa26e2**
(after the extraction commit `8bdfae6ea935fd7d0ce17ebf5b5c1a9434f6f90f`).
The subsequent **b5bc2bd72065d02f0370a3d02b2826310f1a192c** changes only the
browser fixture readiness reader, its tests and documentation. Production
JavaScript, templates, CSS and Go behavior remain identical to `18881ee`.

The production revision passed all **104 distinct browser scenarios** with one
worker, four workers and 104 independent one-scenario invocations. Every run used
zero retries, with zero skipped or flaky scenarios and global exit zero. All
99 baseline titles are retained; five lifecycle/isolation/focus regressions were
added. The final complete Node suite passes **225/225**, without skips. Go
coverage is **76.3%**, above the 73% floor.

The responsibility/lifecycle map, isolation recipe, retained browser contracts
and independent commands are in [frontend isolation](../../frontend-isolation.md).

## Source and environment

- Baseline frontend: exact git archive of main
  `1de9e99173a341719fc28cf764557d4f6f1cc50e` under `/tmp/slu-476-baseline`.
- [Production source manifest](source.json): fingerprint
  `86f7e49cf9c703a45585db4fb61cf349dcb3ac991a2e66c9dd34c30ea4b2ce37`.
  It includes all new adapters, fixtures, specs, unit tests, CSS/templates,
  Playwright configuration, lockfile, fixture/contract Go files, workflows and
  executed helper tools. User prototypes and generated outputs are excluded.
- [Readiness-only follow-up manifest](readiness-source.json) distinguishes the
  final fixture revision from the production matrices. Its four filesystem
  contracts cover missing/empty/partial JSON, accepted ownership, foreign PID
  rejection and non-loopback listener rejection.
- The immutable test executable was built at `8bdfae6`; its SHA-256 is
  `ba5f38610c2847fdf60c1f4c6dafc80f2c11f1ea1bbf208f7493b73d263fb284`.
  The test-only real-router composition in `dashboard_scaling_test.go` is
  unchanged from main. Both screenshot revisions use this identical backend
  fixture; the frontend files are loaded from their respective source trees.
- Linux `6.8.0-147-generic x86_64`, Go `1.26.10`, `GOMAXPROCS=2`, Node `22.23.3`,
  npm `10.9.9`, locked Playwright `1.64.1`, Chromium `156.0.8078.4`
  (`chromium-1248`), staticcheck `2026.1 (v0.7.0)`.
- `npm ci` restored the locked Playwright version; the pre-existing installation
  was `1.63.0`. Repository dependency manifests and release versions are unchanged.

## Actual checks and global exits

Raw logs, cookie-bearing artifacts and intermediate reports remain private under
`/tmp/slu-476-validation`. The checked command arguments, counts, package results
and limits are in [validation.json](validation.json). The reusable
[foreground supervisor](foreground.cjs) starts an independently owned child
session, joins it, and records both exits without sending process-group signals.
[foreground-exits.json](foreground-exits.json) records child **0**, supervisor
**0**, no received signals, and the exact argument lists for all final supervised
runs. Their outer tool sessions also exited **0**.

| Check | Actual result | Private log |
| --- | --- | --- |
| `npm run test:unit` | 225 passed, global 0 | `readiness-unit.log` |
| Full Playwright, one worker, retries 0 | 104 passed, child/supervisor/outer 0 | `accepted-one.log` |
| Full Playwright, four workers, retries 0 | 104 passed, child/supervisor/outer 0 | `accepted-four.log` |
| Every scenario in a separate invocation | 104 discovered, 104 invocations, exactly one pass each; all exits 0 | `accepted-individual.log` and `accepted-individual/summary.json` |
| `go test ./...` | All 25 packages accounted for; global 0 | `accepted-go-test.log` |
| `go vet ./...` | Global 0 | `vet-readiness.log` |
| `go build ./...` | Global 0 | `go-build.log` |
| `go test -race -count=1 ./...` | All 25 packages accounted for; global 0 | `go-race.log` |
| `staticcheck ./...` | Global 0 | `staticcheck.log` |
| Atomic Go coverage | 76.3%; all packages and global 0 | `coverage-qualified.log`, `coverage-qualified-functions.log` |
| CI/release workflow actionlint | Global 0 | `actionlint-qualified.log` |
| Default global setup with no prebuilt executable | 3 auth scenarios passed; global 0 | `default-build.log` |
| Retained #475 `dashboard-e2e.cjs` entry point | 1 scenario passed; browser/global 0, compatible `server.code=0` | `compatibility-preserved.log`, `compatibility-preserved/lifecycle.json` |
| Retired webServer override | Intentional exit 1; supplied command never executed | `retired-override.log` |
| Before/after captures | Six views each, global 0; identical PNG pairs | `qualified-baseline-capture.log`, `accepted-capture.log` |

The Go source tree is identical between the production and readiness revisions;
no backend helper or authentication bypass was added. The Go package results
include 24 tested packages and the helper package with no test files. Coverage
includes that helper's 0%-coverage entry in its total.

The readiness-only follow-up was qualified separately, after the frozen
production matrices completed: **15/15** real auth/startup executions on four
workers (`--repeat-each=5`, retries 0), **4/4** public lifecycle/isolation scenarios
on two workers, and the full **225/225** Node suite. Both browser commands have
child/supervisor/outer exit **0** in `readiness-startup.log` and
`readiness-lifecycle.log`. These are repetitions of existing scenarios, not
additional distinct business coverage.

[scenario-manifest.json](scenario-manifest.json) records retained titles and
counts per domain; [independent-scenarios.json](independent-scenarios.json)
contains only sanitized titles and numeric results for the 104 independent runs.
No empty grep, skipped test, retry or printed success with a nonzero exit counts
as a successful qualification.

## Lifecycle, focus and isolation evidence

- Three successive complete remounts keep one active stream, one action handler,
  one notification/account operation and one onboarding/import handler.
- Twenty Status renders add zero per-row selection listeners. Stable delegated
  containers prevent cleanup registrations from growing with replaced rows.
- Destroy cancels listeners, timers, animation frames, observers and HTTP bodies;
  old success/failure continuations cannot update a new lifetime. External abort
  listeners detach after completion or destruction. Timer identifier namespaces
  remain independent.
- [The conditional-wait audit](async-wait-audit.json) covers 37 additional
  checkpoints before command admission or subsequent effects. Real account,
  typed-backup and public policy-table tests prove an old confirmation resolving
  true after destroy/init cannot admit a command. Confirmation/admission order,
  Approval Scope identity and existing business owners remain unchanged.
- Persisted browser-cache transitions preserve accepted selection and the same
  stream. Actual unload/remount closes the old effects and outstanding dialogs.
- The import result retains its accepted keyboard focus when viewport tracking
  changes the navigation highlight. A newer navigation heading or editor input
  remains focused. The real import workflow, deterministic viewport regression
  and background navigation/editor protection passed **30/30** executions over
  four workers with retries 0 (`import-corrected.log`).
- Simultaneous installations prove distinct PIDs, loopback ports, DB/config/
  known_hosts paths and cookie contexts. Password rotation and session revocation
  in one installation leave the other installation's session and original
  credential valid. Every scenario creates its own normal setup/login state.
- Existing CSP, accessibility, dialog focus traps, responsive shells, approval
  drift, out-of-filter selection, synchronous render snapshots and generation-
  aware completed-offline job-log recovery remain covered by the retained tests.

## Before/after presentation

Both sides use UTC/en-US, reduced motion, fixed browser time
`2026-05-28T12:00:00Z`, the same accepted synthetic facts, and desktop 1440×1000 /
mobile 390×844 viewports. Screenshots capture the complete page. All six pairs
are **byte-for-byte identical**, with zero page exceptions, console errors or
horizontal page overflow in the capture flows.

| View | Before | After |
| --- | --- | --- |
| Status desktop | [PNG](../../../.github/assets/frontend-isolation-476/before/status-desktop.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/status-desktop.png) |
| Status mobile | [PNG](../../../.github/assets/frontend-isolation-476/before/status-mobile.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/status-mobile.png) |
| Admin desktop | [PNG](../../../.github/assets/frontend-isolation-476/before/admin-desktop.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/admin-desktop.png) |
| Admin mobile | [PNG](../../../.github/assets/frontend-isolation-476/before/admin-mobile.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/admin-mobile.png) |
| Manage desktop | [PNG](../../../.github/assets/frontend-isolation-476/before/manage-desktop.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/manage-desktop.png) |
| Manage mobile | [PNG](../../../.github/assets/frontend-isolation-476/before/manage-mobile.png) | [PNG](../../../.github/assets/frontend-isolation-476/after/manage-mobile.png) |

The inspected images contain synthetic hosts and masked example session facts;
no password, token, MFA secret, cookie or user installation is shown.
[screenshots.json](screenshots.json) records dimensions and SHA-256 values;
[presentation.json](presentation.json) records error/overflow observations.

## Rejected iterations and limits

Exploratory runs are explicitly excluded: initial adapter migration failures,
the 102/103 independent run exposing import focus, and outer sessions returning
143 despite printed success. They were replaced by the source-qualified global
0 results above. A redundant login navigation race was removed from the auth
scenario. The confirmed non-atomic readiness-write race was then corrected and
qualified as a fixture-only follow-up. The 104/104 matrices at the earlier
`8bdfae6` commit remain historical evidence, separate from the final production
revision's matrices.

No real SSH/APT host, installed database/config/known_hosts, webhook destination
or production startup worker was used. Test compositions refuse SSH/runner
execution; scenario teardown joins its owned processes and removes temporary
credentials and persistence. User prototypes and existing stashes were preserved.

This validates Chromium on Linux and representative changed-page layouts, not a
cross-browser/device survey or real-host maintenance. Intentional HTTP-error
scenarios exercise failure presentation. No expensive #475 benchmarks were
repeated and no historical performance measurements were relabeled; their
original source provenance remains intact.
