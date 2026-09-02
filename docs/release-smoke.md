# Release Smoke Checklist

[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

Run this checklist before creating a release tag. This is the authoritative disposable-host smoke path: use a disposable app database, disposable `known_hosts`, and a disposable Debian/Ubuntu SSH target. Do not use existing saved inventory entries unless the release owner explicitly confirms they are safe to mutate, and never commit target credentials or smoke artifacts.

For a detailed Codex/Computer Use execution runbook that covers both deterministic UI states and a live disposable-host pass, use [Computer Use Release Checklist](computer-use-release-checklist.md).

## Release source and tag protection

Merge the release commit to `main` and wait for the final `main` CI and CodeQL runs before creating a `vX.Y.Z` tag. Never create a release tag from a release-preparation branch.

The versioned release workflow provides the publication boundary: `release-gate` checks out full history, fetches the current `origin/main`, and uses Git ancestry to require the tagged `GITHUB_SHA` to be part of `origin/main` history. A tag on an unmerged branch or unrelated commit fails before the GitHub release or container jobs can run. The gate repeats the critical normal-CI validations, including frontend unit tests, and every third-party action in the release chain is pinned to a full commit SHA with its readable version recorded in the workflow.

Repository settings provide a separate hosted control. The active `Protect release tags v*` GitHub ruleset prevents updates and deletions of matching tags after creation. This ruleset is not stored in Git and must be verified in **Settings → Rules → Rulesets** before each release if repository administration or ownership changes. The workflow ancestry check remains authoritative for publication even if the hosted ruleset is disabled or removed.

## Preconditions

- Fresh build from the release commit.
- Release commit is merged into `main`, and final `main` CI plus CodeQL are green.
- The `Protect release tags v*` repository ruleset is active and targets `refs/tags/v*`.
- Disposable app DB path and disposable `known_hosts` path.
- One reachable Debian/Ubuntu target host that may be updated, scanned, and have test audit/job records created. Controlled reboot additionally requires explicit approval and a real `systemd` boot environment.
- Target details recorded outside the repo:
  - host and SSH port;
  - username;
  - auth method, either password or private key;
  - sudo behavior, including whether a sudo password is required;
  - confirmation that approving updates is safe.
- Browser access to the app from the tester machine.
- Non-destructive target reachability check completed before the app is pointed at the host, for example an SSH login that only runs `uname -a`, `lsb_release -a` or `/etc/os-release`, and `apt-get -s upgrade`.

Suggested local app command:

```bash
go build -o webserver .
mkdir -p .tmp-smoke
rm -f .tmp-smoke/servers.db .tmp-smoke/known_hosts
: > .tmp-smoke/known_hosts
DEBIAN_UPDATER_DB_PATH=.tmp-smoke/servers.db \
DEBIAN_UPDATER_KNOWN_HOSTS=.tmp-smoke/known_hosts \
./webserver
```

Keep any password, private key, or sudo information outside shell history and outside tracked files. If the target cannot be reached or is no longer confirmed disposable, stop the live smoke and record the exact blocked reason in the smoke result.

## 1) Setup and Login

1. Open `/setup` and create the admin account.
2. Confirm redirect to `/` and authenticated navigation is visible.
3. Click logout and confirm redirect to `/login`.
4. Attempt login with the wrong password and confirm the error banner appears.
5. Login with the correct password and confirm redirect to `/`.

Evidence to capture:

- Screenshot of successful setup redirect.
- Screenshot of invalid login error.

## 2) Add Disposable Host and Trust Key

1. Open `/manage`.
2. Add the disposable target with a clearly disposable name, for example `release-smoke-target`.
3. Confirm missing required fields are rejected before saving the valid host.
4. Scan the host key.
5. Confirm the fingerprint with the release owner or target console.
6. Trust the host key and confirm it is written to the disposable `known_hosts` file.
7. Refresh the page and confirm the host remains saved with secrets hidden.

Evidence to capture:

- Screenshot of saved target row.
- Fingerprint and known-host status.

## 3) Real Update Flow

1. Start an update on the disposable target.
2. Confirm state transitions:
   - `updating` to `pending_approval` when packages are available;
   - `pending_approval` to `upgrading` after approval;
   - final state becomes `done`, `cancelled`, or explicit `error`, never stuck in an active state.
3. If updates are safe, approve the selected release-owner-approved scope.
4. If updates are not safe after scan, cancel the pending update and record the reason.
5. Confirm duplicate action attempts are blocked while the update is active.
6. Confirm the baseline disk, lock, and APT/DPKG health pre-checks are recorded.
7. Confirm the simulated plan records standard/full-upgrade counts and the plan-aware disk requirement before approval. Capture the raw `Need to get` and `After this operation` lines, calculation source and components, filesystem availability, and final pre-check decision.
8. When an upgrade runs, confirm the log records the explicit non-interactive APT strategy and does not emit debconf frontend fallback warnings.
9. Confirm Status shows the appropriate Recommended action before, during, and after maintenance.
10. If reboot is required and explicitly approved, run **Reboot and verify** only on a real disposable `systemd` target and confirm SSH recovery, uptime reset, and the cleared reboot-required marker. Otherwise record the exact skip reason.

Evidence to capture:

- Logs panel showing each transition.
- Final server status.
- Approval or cancel audit event.
- Baseline and plan-aware pre-check result.
- Non-interactive APT strategy line.
- Recommended action and controlled reboot result or skip reason.

## 4) Scheduled Policy Smoke

1. Create a disabled policy first and confirm it does not run.
2. Edit the policy to target only the disposable host using explicit `target_servers`.
3. Use scan-only execution mode unless the release owner explicitly approves scheduled update execution.
4. Confirm the policy list shows the disposable host in matched servers.
5. Select **Canary, then waves**, set a one-host canary, save, and confirm the preview/policy summary retains the rollout settings. With one disposable target, it should be labelled canary; record downstream wave execution as skipped for lack of additional disposable targets.
6. Set the policy time to the next minute in the app timezone, save it, and leave the app running until the scheduler tick passes.
7. Confirm the scheduled run record appears with a clear status and report link.

Evidence to capture:

- Policy summary showing explicit target.
- Stored rollout settings and preview label.
- Scheduled run row and report link.

## 5) Reports, Audit, and Observability

1. Open Manage activity history and filter for the disposable target.
2. Open an audit Markdown report from `/api/reports/audit/:id`.
3. Open a job Markdown report from `/api/reports/jobs/:id`.
4. Open Observability and test `24h`, `7d`, and `30d` windows.
5. Confirm host health trends contain the disposable target after facts refresh or maintenance completion.
6. Confirm dashboard summary panels do not show stale active jobs after the run completes.
7. If a disposable notification receiver is available, configure one destination, send a test, and verify delivery diagnostics. Otherwise record notification delivery as skipped with the exact reason.

Evidence to capture:

- Audit report download.
- Job report download.
- Observability summary screenshot.
- Health-trend result.
- Notification test/diagnostics result or skip reason.

## 6) Backup Export

1. Open `/admin`.
2. Export a backup with a temporary passphrase and include `known_hosts`.
3. Confirm the `.slubkp` file downloads.
4. Verify the downloaded archive with the same passphrase and confirm verification does not change application state.
5. Do not restore over a non-disposable app instance. If restore must be tested, start another temp app DB and restore there.

Evidence to capture:

- Backup export success state.
- Backup verification result.
- Whether `known_hosts` was included.

## 7) Timeout Regression Guard

Run this only against a target or command path that is safe to fail.

1. Stop the app.
2. Restart with `DEBIAN_UPDATER_SSH_COMMAND_TIMEOUT_SECONDS=1` and the same temp DB/known-hosts files.
3. Trigger one safe update or autoremove action expected to exceed the timeout.
4. If the second SSH session confirms an APT/DPKG lock holder, confirm the original command remains attached across repeated liveness checkpoints and the logs include cumulative wait plus lock-holder PIDs.
5. If the lock probe can no longer prove the mutating command outcome, confirm the server enters `needs_reconciliation`, the command is not replayed, and further package mutations are blocked.
6. After proving the original package-manager process is no longer active, run the confirmed **Repair APT** workflow and verify `dpkg --audit` plus `apt-get check` pass before retrying maintenance.
7. Confirm no server remains indefinitely in an active state. A lock-confirmed package process may legitimately remain active beyond several timeout windows while it continues to hold the lock.

Evidence to capture:

- Log excerpt containing timeout/liveness checkpoints and lock-holder evidence.
- Reconciliation and repair result when the outcome becomes uncertain.
- Activity history entry for the failed action.

## 8) Automated Final Gate

Required:

- `go test -count=1 ./...` passes.
- `go vet ./...` passes.
- `staticcheck ./...` passes.
- `govulncheck ./...` passes.
- `actionlint` passes.
- `go test -race -count=1 ./...` passes.
- `go test -covermode=atomic -coverprofile=coverage.out ./...` passes and the coverage summary is recorded.
- `go build -o webserver .` passes.
- `npm ci` succeeds.
- `npm run test:unit` passes.
- `npm audit --audit-level=moderate` passes or reports only accepted advisories documented in the release notes.
- `npm run test:e2e` passes.
- CI (`test (race)`, `test (cover)`, `quality`, `npm-audit`, `frontend-unit`, `ui-e2e`, and `ci-required`) is green on the release commit.

## Smoke Result

Record the result in the release notes or pull request:

- App commit:
- Disposable app DB path:
- Disposable known-hosts path:
- Disposable target name:
- Target OS:
- Target reachability/safety check:
- Update action result:
- Reconciliation/repair result or skip reason:
- Controlled reboot result or skip reason:
- Scheduled policy result:
- Canary/wave result:
- Audit/report result:
- Notification result or skip reason:
- Backup export result:
- Backup verification result:
- Automated gate result:
- Skipped steps and exact reasons:
