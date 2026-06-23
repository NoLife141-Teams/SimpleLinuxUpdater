# Computer Use Release Checklist

[README](../README.md) | [Release smoke](release-smoke.md) | [Production manual QA](production-manual-qa-checklist.md) | [UI manual QA](ui-manual-qa-checklist.md)

Use this runbook when a Codex agent is asked to run a full release smoke with Computer Use before tagging a SimpleLinuxUpdater release. It is an operator checklist, not a CI job: Computer Use drives the local browser and must capture evidence, respect action-time confirmations, and stop when the target is not disposable.

The authoritative release gate remains [Release Smoke Checklist](release-smoke.md). This file is the detailed Computer Use procedure for executing that gate.

## Recent Feature Coverage

This runbook now covers the newer release-smoke surfaces that must be exercised before tagging:

- Notification hooks in Admin, including settings save, event-type selection, redacted webhook payload expectations, and test delivery status.
- Policy dry-run preview in Admin, including matched, excluded, override-disabled, and warning states.
- Maintenance window calendar in Admin, including allowed slots, global/policy no-run windows, overnight windows, timezone display, and optional policy filtering.
- Job detail and audit detail modals, including copy actions and Markdown report links.
- Backup integrity verification through `/api/backup/verify` before any restore attempt.
- Safe bulk-action review modal on the Status dashboard before bulk update, approve, cancel, autoremove, or facts refresh actions.
- Host health trend snapshots in Observability through `/api/observability/health-trends`, including 7-day/30-day windows and optional host filtering.

## Safety Rules

- Use only disposable app databases, disposable `known_hosts` files, and a release-owned disposable Debian/Ubuntu SSH target for live SSH/update steps.
- Never write target credentials, passwords, private keys, sudo passwords, metrics tokens, or backup passphrases into tracked files, screenshots, release notes, PR comments, or terminal history.
- Do not use existing saved inventory entries unless the release owner explicitly confirms they are safe to mutate.
- Do not run backup restore, delete, prune, clear-key, clear-password, sudoers, autoremove, update approval, or host-key clear steps against production data.
- Computer Use must ask for action-time confirmation before destructive UI actions, even during an approved release smoke.
- Computer Use must ask for action-time confirmation before typing sensitive credentials into the app UI, including SSH passwords, sudo passwords, private keys, backup passphrases, and metrics tokens.
- Computer Use must not submit the final successful password-change action. Cover password-change behavior with existing automated tests, or hand the browser to the user for that final submit.
- If the host cannot be reached, the host is not disposable, or update approval is not confirmed safe, stop the affected live step and record the exact blocked reason.

## Runtime Setup

Run all release-smoke runtimes from the repo root. Keep `.tmp-cu-release/` out of commits.

### Deterministic UI Pass

Use this pass to exercise UI states that may not naturally appear on a live target, including pending approval, failures, active runs, CVE badges, reports, audit rows, scheduled runs, and observability summaries.

```bash
go build -o webserver .
mkdir -p .tmp-cu-release/demo
rm -f .tmp-cu-release/demo/servers.db .tmp-cu-release/demo/known_hosts
: > .tmp-cu-release/demo/known_hosts
DEBIAN_UPDATER_DB_PATH=.tmp-cu-release/demo/servers.db \
DEBIAN_UPDATER_KNOWN_HOSTS=.tmp-cu-release/demo/known_hosts \
DEBIAN_UPDATER_DEMO_SEED=variant-c \
DEBIAN_UPDATER_DEMO_RESET=1 \
DEBIAN_UPDATER_SESSION_COOKIE_SECURE=false \
./webserver
```

Open `http://127.0.0.1:8080/setup` with Computer Use and create a temporary release-smoke admin account. Store the password outside the repo.

### Live Disposable-Host Pass

Use this pass for real setup, host-key trust, SSH, update, scheduled policy, backup, report, and metrics behavior.

```bash
go build -o webserver .
mkdir -p .tmp-cu-release/live
rm -f .tmp-cu-release/live/servers.db .tmp-cu-release/live/known_hosts
: > .tmp-cu-release/live/known_hosts
DEBIAN_UPDATER_DB_PATH=.tmp-cu-release/live/servers.db \
DEBIAN_UPDATER_KNOWN_HOSTS=.tmp-cu-release/live/known_hosts \
DEBIAN_UPDATER_SESSION_COOKIE_SECURE=false \
./webserver
```

Before adding the target in the app, complete a non-destructive reachability check outside the UI. Record only non-secret results.

```bash
ssh -p "$SLU_TARGET_PORT" "$SLU_TARGET_USER@$SLU_TARGET_HOST" 'uname -a; if command -v lsb_release >/dev/null 2>&1; then lsb_release -a; else cat /etc/os-release; fi; apt-get -s upgrade'
```

### Optional Restore-Validation Runtime

Use this only after exporting a backup from the live disposable-host pass. Stop the live runtime first, then start a second disposable runtime and restore only into this new DB.

```bash
go build -o webserver .
mkdir -p .tmp-cu-release/restore
rm -f .tmp-cu-release/restore/servers.db .tmp-cu-release/restore/known_hosts
: > .tmp-cu-release/restore/known_hosts
DEBIAN_UPDATER_DB_PATH=.tmp-cu-release/restore/servers.db \
DEBIAN_UPDATER_KNOWN_HOSTS=.tmp-cu-release/restore/known_hosts \
DEBIAN_UPDATER_SESSION_COOKIE_SECURE=false \
./webserver
```

## Deterministic UI Pass Checklist

Record each item as pass, fail, or skipped with the exact reason.

### Auth And Session

- [ ] `/setup` loads on a fresh demo DB.
- [ ] Setup creates the temporary admin and redirects to Status.
- [ ] Authenticated navigation is visible.
- [ ] Logout redirects to `/login`.
- [ ] Browser Back after logout does not reveal protected data.
- [ ] Wrong password shows a clear error and stays on `/login`.
- [ ] Correct login returns to Status.

### Navigation And Layout

- [ ] Status, Manage Servers, Observability, and Admin open from the main navigation.
- [ ] Active nav item is correct on each page.
- [ ] Desktop viewport has no obvious overlapping text, clipped buttons, hidden tables, or unusable controls.
- [ ] Narrow viewport remains usable after scrolling.
- [ ] Page zoom at 90%, 100%, and 125% remains usable when practical in the browser.

### Status Dashboard

- [ ] Top metrics render: total hosts, pending approvals, active runs, failed hosts, security updates, and stale facts.
- [ ] Live state shows either live events or live polling.
- [ ] Search works by server name, host, user, and tag.
- [ ] Status filter, auth filter, grouping, and page size controls update visible rows and counts.
- [ ] Sorting changes row order where sortable columns are available.
- [ ] Selecting one row updates the selected host panel.
- [ ] Select-all on the page updates selected count and bulk actions.
- [ ] Bulk update, approve standard, approve standard security, approve kept-back security, cancel, autoremove, and refresh facts open the safe review modal before executing.
- [ ] Bulk review modal lists selected hosts, eligible actions, blocked hosts, warning text, and confirm/cancel controls.
- [ ] Cancelling the bulk review performs no action.
- [ ] Confirming a safe disposable bulk action executes only the hosts shown as eligible.
- [ ] Maintenance timeline, approval queue, active operations, failures, reboot/risk exposure, audit trail, and command history panels render useful state.
- [ ] Mini-panel buttons open the intended host, drawer, or filtered view.

### Pending Updates Drawer

- [ ] Open a `pending_approval` server from the Status table.
- [ ] Pending updates tab shows package, current version, candidate version, source, security marker, and CVE state.
- [ ] Package count, security count, ready, scanning, unavailable, and skipped badges render when present.
- [ ] Long pending-update lists scroll inside the drawer without scrolling the dashboard behind it.
- [ ] Switching between Logs and Pending updates keeps both tabs usable.
- [ ] Approve all, standard security, kept-back security, full upgrade, and cancel controls are visible or disabled with a useful reason according to the seeded state.
- [ ] Download logs produces a `.txt` file with the expected server name/content.
- [ ] Copy logs copies visible log text when browser permissions allow it.
- [ ] Escape and backdrop close the drawer.

### Manage Servers

- [ ] Manage renders add-server form, global key panel, server table, and activity history.
- [ ] Missing required fields are rejected before a server is saved.
- [ ] Duplicate server name or duplicate host is rejected.
- [ ] Add a disposable UI-only server such as `cu-demo-local`.
- [ ] Edit name, host, port, user, and tags, then refresh and verify persistence.
- [ ] Secrets are hidden in the table and edit flow.
- [ ] Per-server key upload accepts a disposable test key file only after action-time confirmation.
- [ ] Per-server key clear is gated by typed confirmation and only runs on disposable data.
- [ ] Password clear is gated and only runs on disposable data.
- [ ] Known-host scan/trust/clear controls are reachable; skip real trust in the demo pass unless the host is disposable and verified.
- [ ] Delete confirmation blocks incorrect typed confirmation.
- [ ] Delete succeeds only for the disposable UI-only server.
- [ ] Activity history filters by actor, target, action, status, and date range.
- [ ] Audit pagination stays coherent.
- [ ] Audit detail modal opens from an audit row and shows action, target, actor, client IP, request ID, message, sanitized metadata, and formatted time.
- [ ] Audit detail copy action works when browser permissions allow it.
- [ ] Audit report links open or download Markdown from `/api/reports/audit/:id`.
- [ ] Audit prune requires typed confirmation and is skipped unless using disposable demo data.

### Admin

- [ ] App timezone section loads and shows current/resolved timezone.
- [ ] Invalid timezone save shows validation without navigation.
- [ ] Valid timezone such as `America/Toronto` saves and persists.
- [ ] Session status renders.
- [ ] Logout-all typed confirmation blocks incorrect text; do not execute unless the session reset is intentional.
- [ ] Password change form renders current, new, and confirmation fields; do not submit a successful password change with Computer Use.
- [ ] Scheduled policy form accepts target tag, include tags, exclude tags, explicit servers, package scope, execution mode, cadence, weekdays, time, approval timeout, and no-run windows.
- [ ] Policy summary reflects targeting fields.
- [ ] Policy dry-run preview refreshes before save and shows matched hosts, excluded hosts, override-disabled hosts, and warnings.
- [ ] Preview reflects include tags, exclude tags, explicit target servers, package scope, execution mode, and disabled state.
- [ ] Create a disabled scan-only policy against disposable/demo hosts.
- [ ] Edit policy fields and verify saved values reload.
- [ ] Valid blackout JSON applies; invalid JSON shows a clear error.
- [ ] Maintenance Window Calendar panel loads after policy settings.
- [ ] Calendar policy filter shows all policies and a single-policy option where policies exist.
- [ ] Calendar entries show matched servers, allowed scheduled slots, global no-run windows, policy no-run windows, active blocked slots, overnight labels, and app timezone offset.
- [ ] Calendar refresh preserves usable state and reports actionable errors.
- [ ] Policy delete requires typing the policy name and runs only for the disposable policy.
- [ ] Scheduled runs table renders status, summary, target, and report link where present.
- [ ] Job detail modal opens from a scheduled run row and shows status, phase, retry metadata, logs, timestamps, and report URL.
- [ ] Job detail copy logs action works when browser permissions allow it.
- [ ] Job report links open or download Markdown from `/api/reports/jobs/:id`.
- [ ] Notification Hooks panel loads current settings.
- [ ] Invalid webhook URL is rejected without navigation.
- [ ] Enable notification hooks with a disposable HTTPS webhook URL and selected event types.
- [ ] Save persists enabled state, URL, selected event types, and last delivery status.
- [ ] Test notification sends only to the disposable webhook target or stubbed test endpoint.
- [ ] Last delivery status renders event type, status code/result, and timestamp without secrets.

### Backup, Metrics, Observability

- [ ] Backup status loads.
- [ ] Backup export with missing or mismatched passphrase is blocked.
- [ ] Backup export with a temporary passphrase downloads an encrypted `.slubkp` file.
- [ ] Backup restore requires file, passphrase, and typed `RESTORE`; do not restore in the demo runtime unless intentionally testing a throwaway backup.
- [ ] Backup verify accepts a selected `.slubkp` file and passphrase, validates manifest/decryptability, and does not mutate the current app state.
- [ ] Backup verify failure shows a clear non-mutating error for wrong passphrase or invalid file.
- [ ] Metrics token status loads.
- [ ] Generate/rotate token shows the token once.
- [ ] Copy token works when browser permissions allow it.
- [ ] `/metrics` without bearer token is unauthorized or unavailable as expected.
- [ ] `/metrics` with the bearer token returns Prometheus text.
- [ ] Disable metrics token requires typed `DISABLE METRICS`.
- [ ] After disabling, `/metrics` is blocked again.
- [ ] Observability opens and loads KPIs/tables.
- [ ] `24h`, `7d`, and `30d` windows update consistently.
- [ ] Host health trend panel loads sampled-host count, sample count, health problem count, and failure count.
- [ ] Health trend table shows host, latest sample time, package/security deltas, disk free delta, APT status, disk status, and signals.
- [ ] Host trend filter narrows the table to one host and can return to all hosts.
- [ ] Health trend uses `7d` or `30d` data; when the update metrics window is `24h`, the trend panel falls back to the `7d` health window.
- [ ] Empty health trend state is clear when there are no snapshots for the selected host/window.
- [ ] Refresh does not duplicate or stale the UI.

## Live Disposable-Host Pass Checklist

Record each item as pass, fail, or skipped with the exact reason.

### Setup And Host Trust

- [ ] Fresh live DB redirects to `/setup`.
- [ ] Setup/login/logout/wrong-password flow passes as in the demo pass.
- [ ] Add the disposable target with a clearly disposable name, for example `release-smoke-target`.
- [ ] Save rejects missing name, host, or user before accepting valid target details.
- [ ] Refresh shows the target saved with secrets hidden.
- [ ] Scan host key.
- [ ] Confirm fingerprint against the release owner or target console.
- [ ] Trust host key and verify it is written to `.tmp-cu-release/live/known_hosts`.
- [ ] Clear host key is skipped unless intentionally tested against this disposable target.

### Real Server Actions

- [ ] Start update on the disposable target.
- [ ] Duplicate action attempt while active is blocked clearly.
- [ ] Logs begin and status transitions are visible.
- [ ] If packages are available, status reaches `pending_approval`.
- [ ] Pending updates drawer shows real package/version/risk details.
- [ ] If approval is safe, approve the release-owner-approved scope.
- [ ] If approval is not safe, cancel and record the reason.
- [ ] Final state becomes `done`, `cancelled`, or explicit `error`; it never stays indefinitely active.
- [ ] Audit event records approval or cancel.
- [ ] Run apt autoremove only if safe for the disposable target.
- [ ] Autoremove finishes or shows an actionable error.
- [ ] Sudoers enable empty-password attempt is blocked.
- [ ] Sudoers enable/disable runs only when the target and sudo behavior are confirmed disposable and safe.
- [ ] Facts refresh runs and updates host facts or reports an actionable error.

### Scheduled Policy Smoke

- [ ] Create a disabled scan-only policy first and verify it does not run.
- [ ] Edit the policy to target only the disposable host using explicit `target_servers`.
- [ ] Confirm matched servers lists only the disposable target.
- [ ] Policy dry-run preview lists only the disposable target as matched and explains any excluded hosts.
- [ ] Calendar preview for the disposable policy shows the next allowed slot and any active no-run window before the scheduler tick.
- [ ] Set policy time to the next minute in the app timezone.
- [ ] Use scan-only execution mode unless release owner explicitly approves scheduled update execution.
- [ ] Leave app running until scheduler tick passes.
- [ ] Scheduled run row appears with clear status and report link.
- [ ] Job report opens/downloads from `/api/reports/jobs/:id`.
- [ ] Scheduled scan/update completion creates or updates host health trend samples for the disposable target.

### Reports, Backup, Metrics, Observability

- [ ] Manage activity history filters to the disposable target.
- [ ] Audit report opens/downloads from `/api/reports/audit/:id`.
- [ ] Job report opens/downloads from `/api/reports/jobs/:id`.
- [ ] Observability `24h`, `7d`, and `30d` windows show the live run.
- [ ] Observability host health trends show the disposable target in `7d` and `30d` windows after facts refresh or update completion.
- [ ] Host health trend host filter shows only the disposable target when selected.
- [ ] Dashboard summary panels do not show stale active jobs after completion.
- [ ] Export backup with a temporary passphrase and include `known_hosts`.
- [ ] Verify exported backup with the same passphrase before attempting any restore.
- [ ] `.slubkp` file downloads.
- [ ] Optional restore is performed only in the separate restore-validation runtime.
- [ ] Metrics token generate/authorized scrape/disable flow passes without recording the token.

## Automated Final Gate

Run these from the release commit and record pass/fail output. If a tool is not installed, install/use the repo-standard pinned version or record the exact blocker.

- [ ] `go test -count=1 ./...`
- [ ] `go vet ./...`
- [ ] `staticcheck ./...`
- [ ] `govulncheck ./...`
- [ ] `actionlint`
- [ ] `go test -count=1 -run 'TestBackendContractRouteGroups|TestBackendContractReportsAndPolicies|TestRegisterRoutesInventory' ./`
- [ ] `go test -count=1 ./internal/notifications ./internal/policies ./internal/observability ./internal/updates`
- [ ] `go test -race -count=1 ./...`
- [ ] `go test -covermode=atomic -coverprofile=coverage.out ./...`
- [ ] `go tool cover -func=coverage.out | tail -n 1`
- [ ] `go build -o webserver .`
- [ ] `npm audit --audit-level=moderate`
- [ ] `npm run test:e2e`
- [ ] Remove generated `coverage.out` before committing release-prep changes, unless the release owner explicitly asks to keep it.
- [ ] Release-commit CI is green for `test (unit)`, `test (race)`, `test (cover)`, `ui-e2e`, `quality`, and `npm-audit`.

## Smoke Result Template

Copy this result into the release PR or release notes. Do not include secrets.

```markdown
## Computer Use Release Smoke Result

- App commit:
- Branch:
- Date/time:
- Browser:
- Computer Use operator:
- Demo DB path: `.tmp-cu-release/demo/servers.db`
- Demo known_hosts path: `.tmp-cu-release/demo/known_hosts`
- Live DB path: `.tmp-cu-release/live/servers.db`
- Live known_hosts path: `.tmp-cu-release/live/known_hosts`
- Restore DB path, if used: `.tmp-cu-release/restore/servers.db`
- Disposable target name:
- Target OS:
- Target host/port recorded outside repo: yes/no
- Target credentials recorded outside repo: yes/no
- Target reachability/safety check:

### Pass Summary

- Deterministic UI pass:
- Live disposable-host pass:
- Auth/session:
- Navigation/layout:
- Status dashboard:
- Pending updates drawer:
- Manage Servers:
- Server actions:
- Admin:
- Backup/restore:
- Metrics:
- Observability/audit/reports/health trends:
- Notifications:
- Policy preview/calendar:
- Bulk action review:
- Automated final gate:
- CI gate:

### Evidence

- Screenshots:
- Downloaded audit reports:
- Downloaded job reports:
- Backup export filename:
- Metrics check result:
- Known-host fingerprint/status:
- Update action result:
- Scheduled policy result:

### Skips And Blockers

- Skipped steps and exact reasons:
- Failures:
- Follow-up issues:
```
