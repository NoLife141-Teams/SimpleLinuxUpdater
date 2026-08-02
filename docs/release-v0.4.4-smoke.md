# v0.4.4 Release Smoke Result

This result records the pre-tag disposable-host validation performed for
`v0.4.4`.

## Scope

- Date: 2026-08-02 America/Toronto
- Release implementation commit: `f2b9990`
- Release preparation branch: `codex/release-v0.4.4`
- Candidate image:
  `sha256:d8ad53263ac0d373e4bf802ee8b44cf80e259280adcbb4c493116a9cb41e05c7`
- Disposable app container: `slu-v044-rc`
- Disposable target: `release-smoke-target`
- Target OS: Ubuntu 24.04.4 LTS container

Credentials, private keys, backup passphrases, and session cookies were kept out
of the repository and this result.

## Host Trust And Update Flow

- A non-destructive SSH preflight confirmed reachability, passwordless sudo,
  and a successful `apt-get -s upgrade`.
- The target host-key fingerprint was matched against the target console before
  trust was recorded.
- Baseline disk, APT lock, and APT/DPKG health checks passed.
- The simulated plan reported 13 standard security updates, no new packages,
  and a plan-aware reserve of 1.81 GiB; 886.46 GiB was free.
- The log recorded the explicit non-interactive frontend, disabled changelogs,
  automatic `needrestart`, and keep-existing-conffile strategy.
- The release owner approved the 13 security updates. All 13 installed.
- The first post-check reported only the disposable image's missing `systemctl`.
  A narrow test harness was then installed that succeeds exclusively for
  `systemctl --failed`; `apt-get check` passed, and the next app scan completed
  successfully.
- Final Status was Healthy with APT health passing, no pending updates, and no
  reboot required.

The controlled reboot step was skipped because a minimal container is not a
real `systemd` boot environment and cannot prove uptime reset or SSH recovery.
The live ambiguous-timeout/reconciliation scenario was also skipped: forcing an
uncertain mutation immediately after a real package upgrade would add risk
without improving on the deterministic timeout, lock-holder, reconciliation,
and repair coverage in the automated gate.

## Scheduled Canary And Waves

- A disabled scan-only policy named `v0.4.4 release canary` targeted only
  `release-smoke-target`.
- The preview labelled the target as canary and retained a one-host canary,
  waves of 10, and a 15-minute delay.
- Enabling the next-minute schedule produced one catch-up run and the current
  occurrence. Both completed in 4 seconds with `succeeded` and
  `Scheduled scan completed: no pending updates`.
- Each scheduled run exposed job details, a job report link, and a filtered
  audit link.
- The policy was disabled again after validation.

Downstream waves were skipped because the disposable fleet contained only the
single canary target.

## Backup, Audit, And Observability

- An encrypted backup export included `servers.db`, `config.json`, and
  `known_hosts`.
- Read-only restore verification reported `Ready for confirmation`, archive
  format version 1, no blockers, and the expected one server, one policy, three
  jobs, and one archived session.
- No restore was submitted, so application state was not replaced.
- Backup recovery health recorded both a successful export and a successful
  verification.
- Manage audit history recorded successful `backup.export`, `backup.verify`,
  `update.complete`, and scheduled-run events with report links.
- Observability exposed the 24-hour, 7-day, and 30-day windows, showed the
  disposable target in host health trends, and reflected the completed runs.

Chrome blocked only the HTTP download's final filename confirmation as an
insecure download. The already-downloaded encrypted bytes were selected for the
read-only verification, which passed; the warning was not bypassed.

## Intentionally Skipped

- Notification delivery: no disposable external receiver was configured.
- Metrics-token lifecycle: generating persistent access was unrelated to the
  release delta and remains covered by automated tests.
- Backup restore: verification was sufficient for the live instance; restore
  was not needed and would replace the disposable runtime's state.
- Controlled reboot: the target was not a real `systemd` boot environment.
- Additional rollout waves: there was only one disposable target.

## Automated And Docker Gates

The full local gate passed with 148 frontend unit tests, 60 Playwright tests,
73.1% Go coverage, zero npm vulnerabilities, no reachable Go vulnerability,
and successful vet, static analysis, race, build, and Actions-workflow checks.
The Docker candidate also passed non-root execution, database ownership and
mode, fresh setup, and restart persistence checks. Full command and image
details are recorded in
[v0.4.4 Release Readiness](release-v0.4.4-readiness.md).
