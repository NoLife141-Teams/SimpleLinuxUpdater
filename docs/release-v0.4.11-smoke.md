# v0.4.11 Release Smoke Result

## Scope

- Date: 2026-09-09 America/Toronto (2026-09-10 UTC).
- Implementation: `5ffb011d907138283ba734b49dc11f1503d86ec6` plus the
  policy-preview correction in `cd9fd9e`, with v0.4.11 release metadata.
- Application: macOS arm64, Go 1.26.6, isolated upgrade and restore databases
  and separate disposable `known_hosts` files.
- Upgrade source: official, checksum-verified v0.4.10 macOS arm64 archive.
- SSH targets: two release-owned Debian 12.15 arm64 containers, non-root
  `smoke` accounts, loopback-only published ports, fresh credentials and keys,
  and `SYS_PTRACE` for the APT lock-holder checks.
- Browser coverage: 71 Playwright tests. Live host, scheduler, migration and
  restore checks used HTTP requests against the real application; no mocked
  SSH or scheduler responses and no accelerated application clock.

## Upgrade and real maintenance

- Initialized v0.4.10 with an existing account, a password-authenticated server,
  a private-key-authenticated server, tags, trusted host keys, a disabled policy,
  two completed helper jobs and 15 audit events.
- Verified each host fingerprint against its own container console. **Enable
  apt** installed the typed helper on both hosts; real host-fact refresh worked.
- Stopped v0.4.10 and opened the same database with the candidate. Existing
  account login succeeded, both servers started enabled, encrypted inventory
  fields and `known_hosts` were unchanged, and policies/jobs/audit history were
  retained. Both SSH authentication methods still worked without reinstalling
  the helper.
- A real update reached pending approval. Duplicate update, stale approval
  generation, and disabling while approval was pending were rejected with 409.
- Approval with the displayed job/generation completed the package update:
  `libpcre2-8-0` changed from `10.42-1` to `10.42-1+deb12u1`. The persisted job
  succeeded and its Markdown report, Observability summary and health-trend
  APIs were readable.

## Two-host scheduling and availability

All times in this section are UTC. Policies used scan-only mode, a one-host
canary, one downstream host and a one-minute wave delay.

| Scenario | Result |
| --- | --- |
| Disabled `release-b` | Manual update, autoremove and facts refresh returned 409. Preview matched only `release-a`, and only that host received a scheduled job. |
| Re-enabled `release-b` | Real facts refresh succeeded and both hosts returned to policy eligibility. |
| Successful canary/wave | Both scans succeeded in order, with the original occurrence preserved in run and audit records. |
| Failed canary followed by disabling it | The downstream host was skipped with `rollout_gate`; no downstream job was created. |

For the successful policy (ID 3), both runs retained the occurrence
`2026-09-10T02:52:00Z`:

- Canary `release-a`: started `02:52:52.155Z`, finished `02:52:59.202Z`, job
  `ace9c673f1bcfdf5d26da18edc4fb9c6`.
- Wave `release-b`: started `02:53:52.149Z`, finished `02:53:56.129Z`, job
  `4ed405d9da7f99d658d8801c47d30684`.

For the failure policy (ID 4), the canary's SSH container was stopped before
its `02:56` occurrence. After its failed run was visible, the canary was disabled
through the real availability API. The `02:57` scheduler tick still skipped the
remaining host with `rollout_gate` and an empty job ID. All smoke policies were
disabled afterwards.

## Restart, automatic refresh and backup/restore

- Disabled state survived a complete application restart.
- With stale/missing facts prepared only in the stopped disposable database,
  the real automatic worker refreshed the enabled host and left the disabled
  host's stale facts untouched. Audit metadata identified `automatic_periodic`
  only for the enabled host. Re-enabling the second host and restarting the
  worker allowed its automatic refresh to complete.
- Exported an encrypted backup containing one enabled and one disabled server,
  four policies, nine jobs, 75 audit events and `known_hosts`. Verification
  succeeded with the correct passphrase and rejected a wrong passphrase.
- Restored into a separate initialized runtime containing a sentinel inventory
  entry. Restore invalidated its old session, accepted the restored account,
  replaced the sentinel inventory, retained disabled state and history, and
  released maintenance without requiring a restart.
- Restore intentionally re-encrypted secrets under the destination encryption
  key. Password and private-key SSH authentication both worked after restoration;
  disabled maintenance remained blocked until the server was re-enabled.

## Defect found and corrected

The first live policy preview included the disabled host and assigned it a wave.
Action admission and scheduler matching already rejected that host; preview had
its own exclusion path without the availability check. A focused Go regression
failed on both the extra match and its rollout stage before the fix. The corrected
preview excludes disabled hosts from matches and occurrence counts, explains
`server disabled` in the browser, and returns the host after re-enabling it.
The focused regression, live API scenario and full Go/browser suites passed.

## Environment limitations and harness corrections

The targets have no systemd PID 1. Only the disposable application used
`DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_FAILED_UNITS=false`; unavailable systemd health
remained a warning, while APT and disk checks remained enforced. Controlled reboot
and real systemd service-health behavior were not qualified. External notification
delivery was not exercised. Timeout ambiguity and cross-midnight rollout cases
remain covered by deterministic regression tests.

The harness was corrected to read `schedule.run.completed` and decode `meta_json`
when checking audits, and to validate usable restored credentials rather than
expecting identical ciphertext across destination keys. Restarting the SSH fixture
changed its ephemeral port and host key; the disposable inventory was updated and
the new fingerprint independently checked against the container console before
continuing. None of these corrections relaxed application checks.

Temporary credentials, cookies, databases, backups and raw diagnostics are outside
tracked files. No production host, production database, tag or registry publication
was used. Disposable applications and containers were stopped after validation.
The two SSH target containers and archive verifier remain stopped locally; no
Docker smoke volumes remain.
