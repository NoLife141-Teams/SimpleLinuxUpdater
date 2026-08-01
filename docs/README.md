# Documentation map

This index separates current guidance from historical implementation records. Product behavior is defined by the current code and active guides; archived plans, evidence captures, and versioned release results describe the state at the time they were written.

## User and operator guides

- [Installation](installation.md): supported installation paths and target requirements.
- [Configuration](configuration.md): environment variables, storage, sessions, notifications, retries, and health checks.
- [Usage](usage.md): server maintenance, approvals, repair, reboot, policies, notifications, observability, and backup workflows.
- [Deployment](deployment.md): Docker/binary deployment, HTTPS, metrics, upgrades, and persistence.
- [Security](security.md): threat model, credentials, notification destinations, sudo, and hardening.
- [Troubleshooting](troubleshooting.md): recovery guidance for authentication, SSH, APT, reboot, CVE, notifications, and storage.

## Architecture and contribution

- [Architecture](architecture.md): current runtime, service boundaries, persistence, and maintenance lifecycle.
- [Contributing](contributing.md): contributor workflow, domain ownership, and validation commands.
- [`CONTEXT.md`](../CONTEXT.md): canonical domain vocabulary.
- [`AGENTS.md`](../AGENTS.md) and [`docs/agents/`](agents/): repository instructions for coding agents, issue tracking, and triage.

## Validation and release operations

- [Release smoke checklist](release-smoke.md): authoritative disposable-host release gate.
- [Computer Use release checklist](computer-use-release-checklist.md): detailed deterministic UI and live-host execution procedure.
- [Production manual QA](production-manual-qa-checklist.md): non-destructive exploration of an existing deployment.
- [UI manual QA](ui-manual-qa-checklist.md): disposable local UI checks without requiring a real SSH target.

## Historical records

- `release-v*-readiness.md` and `release-v*-smoke.md` are immutable evidence for specific releases.
- [`plans/`](plans/) contains implementation plans and design records; consult [Architecture](architecture.md) and the current code for shipped behavior.
- [`archive/`](archive/) contains superseded plans and one-time stabilization records.
- [Operator application shell evidence](operator-application-shell-evidence.md) and [UI quality evidence](ui-quality-evidence.md) are dated visual evidence, not current product specifications.
- [`CHANGELOG.md`](../CHANGELOG.md) is the chronological release history; released entries are not rewritten when behavior later changes.
